package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

var erroredUpstream = errors.New("errored upstream")

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
	retry := 0
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

			res, err := http.DefaultClient.Do(req)
			if err != nil {
				return err
			}
			defer res.Body.Close()

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

var paymentClient = &http.Client{Timeout: 10 * time.Second}

// 決済を成功するまで再試行する。Idempotency-Key（ライドID）を付けるので、
// 前の試行が実は成功していても二重には請求されない（マニュアル記載の仕様）。
func postPaymentUntilSuccess(ctx context.Context, paymentGatewayURL, token, idempotencyKey string, param *paymentGatewayPostPaymentRequest) error {
	b, err := json.Marshal(param)
	if err != nil {
		return err
	}
	backoff := 20 * time.Millisecond
	for {
		err := func() error {
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, paymentGatewayURL+"/payments", bytes.NewBuffer(b))
			if err != nil {
				return err
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Idempotency-Key", idempotencyKey)
			res, err := paymentClient.Do(req)
			if err != nil {
				return err
			}
			defer res.Body.Close()
			io.Copy(io.Discard, res.Body)
			if res.StatusCode != http.StatusNoContent {
				return fmt.Errorf("[POST /payments] unexpected status code (%d)", res.StatusCode)
			}
			return nil
		}()
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("payment did not succeed: %w (last: %v)", ctx.Err(), err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 500*time.Millisecond)
	}
}
