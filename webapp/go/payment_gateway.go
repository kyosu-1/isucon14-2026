package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
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

func requestPaymentGatewayPostPayment(ctx context.Context, paymentGatewayURL string, token string, idempotencyKey string, param *paymentGatewayPostPaymentRequest, retrieveRidesOrderByCreatedAtAsc func() ([]Ride, error)) error {
	b, err := json.Marshal(param)
	if err != nil {
		return err
	}

	// 失敗したらとりあえずリトライ
	// FIXME: 社内決済マイクロサービスのインフラに異常が発生していて、同時にたくさんリクエストすると変なことになる可能性あり
	started := time.Now()
	retry := 0
	defer func() { paymentStats.observe(time.Since(started), retry) }()
	for {
		err := func() error {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, paymentGatewayURL+"/payments", bytes.NewBuffer(b))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			// 同じライドの支払いは何度送っても1回として扱われる（マニュアル: Idempotency-Key）
			req.Header.Set("Idempotency-Key", idempotencyKey)

			attemptStart := time.Now()
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				paymentStats.attempt(-1, time.Since(attemptStart))
				return err
			}
			defer res.Body.Close()
			paymentStats.attempt(res.StatusCode, time.Since(attemptStart))

			if res.StatusCode != http.StatusNoContent {
				// エラーが返ってきても成功している場合があるので、社内決済マイクロサービスに問い合わせ
				getReq, err := http.NewRequestWithContext(ctx, http.MethodGet, paymentGatewayURL+"/payments", bytes.NewBuffer([]byte{}))
				if err != nil {
					return err
				}
				getReq.Header.Set("Authorization", "Bearer "+token)

				getRes, err := http.DefaultClient.Do(getReq)
				if err != nil {
					return err
				}
				defer res.Body.Close()

				// GET /payments は障害と関係なく200が返るので、200以外は回復不能なエラーとする
				if getRes.StatusCode != http.StatusOK {
					return fmt.Errorf("[GET /payments] unexpected status code (%d)", getRes.StatusCode)
				}
				var payments []paymentGatewayGetPaymentsResponseOne
				if err := json.NewDecoder(getRes.Body).Decode(&payments); err != nil {
					return err
				}

				rides, err := retrieveRidesOrderByCreatedAtAsc()
				if err != nil {
					return err
				}

				if len(rides) != len(payments) {
					return fmt.Errorf("unexpected number of payments: %d != %d. %w", len(rides), len(payments), erroredUpstream)
				}

				return nil
			}
			return nil
		}()
		if err != nil {
			if retry < 5 {
				retry++
				time.Sleep(100 * time.Millisecond)
				continue
			} else {
				return err
			}
		}
		break
	}

	return nil
}
