package main

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// ライドの状態遷移の DB 書き込みを1本の FIFO にまとめて、バッチで書く。
//
// 椅子側の遷移（ENROUTE / PICKUP / CARRYING / ARRIVED）はメモリに反映した時点で応答し、DB は待たない。
// 1件ずつ同期でトランザクションを張ると 1リクエスト ~100ms かかり（DB busy 75%）、
// 負荷終了の瞬間に一斉に送られた ENROUTE がまとめて切断されて WARN（EOF）になっていた。
//
// 評価（COMPLETED）は同じ FIFO に入れて書き込み完了を待つ。FIFO なので、それより前の遷移は
// 必ず先に DB に入っており、owner/sales（DB を読む）にも評価の応答前に反映される。
type rideWrite struct {
	statusID   string
	rideID     string
	status     string
	at         time.Time // 遷移した時刻（ride_statuses.created_at）。割り当てのときは割り当て時刻
	evaluation int       // COMPLETED のときだけ
	chairID    string    // マッチングの割り当てのときだけ（status は空）
	done       chan error
}

var rideWriter = struct {
	mu   sync.Mutex
	cond *sync.Cond
	q    []*rideWrite
	gen  int // initialize のたびに進める。古い世代の書き込みは捨てる
}{}

func init() {
	rideWriter.cond = sync.NewCond(&rideWriter.mu)
}

var errDiscarded = errors.New("discarded by initialize")

func enqueueRideWrite(w *rideWrite) {
	rideWriter.mu.Lock()
	rideWriter.q = append(rideWriter.q, w)
	rideWriter.mu.Unlock()
	rideWriter.cond.Signal()
}

// initialize で呼ぶ。まだ書いていない前回ベンチの遷移を捨てる（作り直した DB に書かない）。
func discardRideWrites() {
	rideWriter.mu.Lock()
	q := rideWriter.q
	rideWriter.q = nil
	rideWriter.gen++
	rideWriter.mu.Unlock()
	for _, w := range q {
		if w.done != nil {
			w.done <- errDiscarded
		}
	}
}

func startRideWriter() {
	go func() {
		for {
			rideWriter.mu.Lock()
			for len(rideWriter.q) == 0 {
				rideWriter.cond.Wait()
			}
			batch := rideWriter.q
			rideWriter.q = nil
			gen := rideWriter.gen
			rideWriter.mu.Unlock()

			// initialize と排他（書いている途中でテーブルを作り直されないように）。
			// 取り出した後に initialize が走っていたら、そのバッチは前回ベンチのものなので捨てる
			flushMu.Lock()
			rideWriter.mu.Lock()
			stale := gen != rideWriter.gen
			rideWriter.mu.Unlock()
			var err error
			if stale {
				err = errDiscarded
			} else {
				err = writeRideBatch(context.Background(), batch)
			}
			flushMu.Unlock()
			if err != nil && !stale {
				slog.Error("ride writer", "err", err, "n", len(batch))
			}
			for _, w := range batch {
				if w.done != nil {
					w.done <- err
				}
			}
		}
	}()
}

func writeRideBatch(ctx context.Context, batch []*rideWrite) error {
	for len(batch) > 0 {
		n := min(len(batch), 500)
		if err := writeRideChunk(ctx, batch[:n]); err != nil {
			return err
		}
		batch = batch[n:]
	}
	return nil
}

func writeRideChunk(ctx context.Context, batch []*rideWrite) error {
	tx, err := db.BeginTxx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	assigns := make([]*rideWrite, 0)
	statuses := make([]*rideWrite, 0, len(batch))
	for _, w := range batch {
		if w.chairID != "" {
			assigns = append(assigns, w)
		} else {
			statuses = append(statuses, w)
		}
	}

	// 0. マッチングの割り当て。FIFO なので、同じライドのこれ以降の状態遷移・評価より必ず先に書かれる
	//    （評価で決まる updated_at = 完了日時を、遅れて来た割り当てが上書きすることはない）
	if len(assigns) > 0 {
		args := make([]any, 0, len(assigns)*5)
		q := "UPDATE rides SET updated_at = CASE id"
		for _, w := range assigns {
			q += " WHEN ? THEN ?"
			args = append(args, w.rideID, w.at)
		}
		q += " END, chair_id = CASE id"
		for _, w := range assigns {
			q += " WHEN ? THEN ?"
			args = append(args, w.rideID, w.chairID)
		}
		q += " END WHERE id IN (?" + strings.Repeat(",?", len(assigns)-1) + ") AND chair_id IS NULL"
		for _, w := range assigns {
			args = append(args, w.rideID)
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			return err
		}
	}
	if len(statuses) == 0 {
		return tx.Commit()
	}

	// 1. 履歴
	args := make([]any, 0, len(statuses)*4)
	for _, w := range statuses {
		args = append(args, w.statusID, w.rideID, w.status, w.at)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO ride_statuses (id, ride_id, status, created_at) VALUES "+
			strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?),", len(statuses)), ","),
		args...); err != nil {
		return err
	}

	// 2. 最新状態（同じライドが複数あれば FIFO の最後）。updated_at は据え置く
	last := make(map[string]string, len(statuses))
	order := make([]string, 0, len(statuses))
	for _, w := range statuses {
		if _, ok := last[w.rideID]; !ok {
			order = append(order, w.rideID)
		}
		last[w.rideID] = w.status
	}
	args = args[:0]
	q := "UPDATE rides SET status = CASE id"
	for _, id := range order {
		q += " WHEN ? THEN ?"
		args = append(args, id, last[id])
	}
	q += " END, updated_at = updated_at WHERE id IN (?" + strings.Repeat(",?", len(order)-1) + ")"
	for _, id := range order {
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx, q, args...); err != nil {
		return err
	}

	// 3. 評価（完了日時 = updated_at は評価した時刻）
	for _, w := range statuses {
		if w.status != "COMPLETED" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rides SET evaluation = ?, updated_at = ? WHERE id = ?`, w.evaluation, w.at, w.rideID); err != nil {
			return err
		}
	}
	return tx.Commit()
}
