package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

var erroredUpstream = errors.New("errored upstream")

// 計測: 決済呼び出しの所要時間と再試行回数を5秒ごとに1行ログに出す
var paymentStats = &paymentStatsT{}

type paymentStatsT struct {
	sync.Mutex
	lastLog  time.Time
	n        int
	sum      time.Duration
	max      time.Duration
	retries  int
	codes    map[int]int
	codeTime map[int]time.Duration
}

// 1回の POST /payments の結果（code=-1 は通信エラー）
func (p *paymentStatsT) attempt(code int, d time.Duration) {
	p.Lock()
	defer p.Unlock()
	if p.codes == nil {
		p.codes, p.codeTime = map[int]int{}, map[int]time.Duration{}
	}
	p.codes[code]++
	p.codeTime[code] += d
}

func (p *paymentStatsT) observe(d time.Duration, retries int) {
	p.Lock()
	defer p.Unlock()
	p.n++
	p.sum += d
	p.max = max(p.max, d)
	p.retries += retries
	if time.Since(p.lastLog) >= 5*time.Second {
		attempts := ""
		for c, n := range p.codes {
			attempts += fmt.Sprintf(" %d:%dx%dms", c, n, p.codeTime[c].Milliseconds()/int64(n))
		}
		slog.Info("payment", "n", p.n, "avg_ms", p.sum.Milliseconds()/int64(p.n), "max_ms", p.max.Milliseconds(), "retries", p.retries, "attempts", attempts)
		p.lastLog = time.Now()
		p.n, p.sum, p.max, p.retries = 0, 0, 0, 0
		p.codes, p.codeTime = map[int]int{}, map[int]time.Duration{}
	}
}

type paymentGatewayPostPaymentRequest struct {
	Amount int `json:"amount"`
}

type paymentGatewayGetPaymentsResponseOne struct {
	Amount int    `json:"amount"`
	Status string `json:"status"`
}

// 決済サーバーは約7割の確率で 500/502/504 を返す（1回 ~35ms）。
// 元の実装は失敗のたびに 100ms 待ち + GET /payments で件数を照合していて、1件 avg 0.6〜0.8s かかっていた。
// Idempotency-Key（ライドID）を付けているので、前の試行が実は成功していても同じキーの再送は二重請求にならない。
// だから失敗したら待たずに POST を送り直すだけでよい。
var paymentClient = &http.Client{
	Timeout: 10 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        512,
		MaxIdleConnsPerHost: 512,
		IdleConnTimeout:     90 * time.Second,
	},
}

// 決済を同じ Idempotency-Key で paymentParallel 本同時に再試行し続け、最初に成功した時点で返す。
// 決済サーバーは1回 ~35ms で、成功は約3割（残りは 500/502/504）。1本で順に再試行すると平均 3.3回 ≒ 130ms。
// 3本並べると最初の成功までは平均 ~1.5回ぶん。同じキーの送信は何回でも1回の支払いとして扱われる（マニュアル）。
// 成功が決まったら残りの本は新しい試行を始めない（送信中のものは切らずに流す: 切るとコネクションを張り直すため）。
const paymentParallel = 3

func requestPaymentGatewayPostPayment(ctx context.Context, paymentGatewayURL string, token string, idempotencyKey string, param *paymentGatewayPostPaymentRequest, _ func() ([]Ride, error)) error {
	b, err := json.Marshal(param)
	if err != nil {
		return err
	}

	started := time.Now()
	var attempts atomic.Int64
	defer func() { paymentStats.observe(time.Since(started), int(attempts.Load())-1) }()
	deadline := started.Add(8 * time.Second)

	// 成功したかどうかは done だけで判断する（試行せずに抜けた本が「エラーなし」で成功扱いにならないように）
	var done atomic.Bool
	result := make(chan error, paymentParallel)
	for w := 0; w < paymentParallel; w++ {
		go func() {
			last := fmt.Errorf("payment not attempted: %w", erroredUpstream)
			for !done.Load() {
				if err := ctx.Err(); err != nil {
					last = err
					break
				}
				if time.Now().After(deadline) {
					break
				}
				attempts.Add(1)
				if err := postPaymentOnce(ctx, paymentGatewayURL, token, idempotencyKey, b); err != nil {
					last = err
					continue
				}
				done.Store(true)
			}
			result <- last
		}()
	}
	var lastErr error
	for w := 0; w < paymentParallel; w++ {
		lastErr = <-result
		if done.Load() {
			return nil
		}
	}
	return lastErr
}

func postPaymentOnce(ctx context.Context, paymentGatewayURL, token, idempotencyKey string, body []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, paymentGatewayURL+"/payments", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	// 同じライドの支払いは何度送っても1回として扱われる（マニュアル: Idempotency-Key）
	req.Header.Set("Idempotency-Key", idempotencyKey)

	attemptStart := time.Now()
	res, err := paymentClient.Do(req)
	if err != nil {
		paymentStats.attempt(-1, time.Since(attemptStart))
		return err
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	paymentStats.attempt(res.StatusCode, time.Since(attemptStart))
	if res.StatusCode != http.StatusNoContent {
		return fmt.Errorf("[POST /payments] unexpected status code (%d): %w", res.StatusCode, erroredUpstream)
	}
	return nil
}
