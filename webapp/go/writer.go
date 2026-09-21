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
	at         time.Time // 遷移した時刻（ride_statuses.created_at）
	evaluation int       // COMPLETED のときだけ
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

	// 1. 履歴
	args := make([]any, 0, len(batch)*4)
	for _, w := range batch {
		args = append(args, w.statusID, w.rideID, w.status, w.at)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO ride_statuses (id, ride_id, status, created_at) VALUES "+
			strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?),", len(batch)), ","),
		args...); err != nil {
		return err
	}

	// 2. 最新状態（同じライドが複数あれば FIFO の最後）。updated_at は据え置く
	last := make(map[string]string, len(batch))
	order := make([]string, 0, len(batch))
	for _, w := range batch {
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
	for _, w := range batch {
		if w.status != "COMPLETED" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE rides SET evaluation = ?, updated_at = ? WHERE id = ?`, w.evaluation, w.at, w.rideID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// 椅子の登録（INSERT chairs）と稼働状態（UPDATE chairs.is_active）の DB 書き込みも FIFO にまとめて後から書く。
// 読み取りはすべてメモリ（authCache / st.chairs）からなので、応答は DB を待たない。
// 同じ FIFO なので、ある椅子の is_active の更新は必ずその椅子の INSERT の後に書かれる。
type chairWrite struct {
	insert   *Chair // 登録
	activeID string // 稼働状態の変更
	active   bool
}

var chairWriter = struct {
	mu   sync.Mutex
	cond *sync.Cond
	q    []chairWrite
	gen  int
}{}

func init() {
	chairWriter.cond = sync.NewCond(&chairWriter.mu)
}

func enqueueChairWrite(w chairWrite) {
	chairWriter.mu.Lock()
	chairWriter.q = append(chairWriter.q, w)
	chairWriter.mu.Unlock()
	chairWriter.cond.Signal()
}

// initialize で呼ぶ。前回ベンチの椅子を作り直した DB に書かない。
func discardChairWrites() {
	chairWriter.mu.Lock()
	chairWriter.q = nil
	chairWriter.gen++
	chairWriter.mu.Unlock()
}

func startChairWriter() {
	go func() {
		for {
			chairWriter.mu.Lock()
			for len(chairWriter.q) == 0 {
				chairWriter.cond.Wait()
			}
			batch := chairWriter.q
			chairWriter.q = nil
			gen := chairWriter.gen
			chairWriter.mu.Unlock()

			flushMu.Lock()
			chairWriter.mu.Lock()
			stale := gen != chairWriter.gen
			chairWriter.mu.Unlock()
			if !stale {
				if err := writeChairBatch(context.Background(), batch); err != nil {
					slog.Error("chair writer", "err", err, "n", len(batch))
				}
			}
			flushMu.Unlock()
		}
	}()
}

func writeChairBatch(ctx context.Context, batch []chairWrite) error {
	var inserts []*Chair
	active := make(map[string]bool) // 同じ椅子が複数あれば FIFO の最後
	for _, w := range batch {
		if w.insert != nil {
			inserts = append(inserts, w.insert)
		} else {
			active[w.activeID] = w.active
		}
	}
	for len(inserts) > 0 {
		n := min(len(inserts), 500)
		args := make([]any, 0, n*8)
		for _, c := range inserts[:n] {
			args = append(args, c.ID, c.OwnerID, c.Name, c.Model, false, c.AccessToken, c.CreatedAt, c.UpdatedAt)
		}
		if _, err := db.ExecContext(ctx,
			"INSERT INTO chairs (id, owner_id, name, model, is_active, access_token, created_at, updated_at) VALUES "+
				strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?, ?, ?, ?),", n), ","),
			args...); err != nil {
			return err
		}
		inserts = inserts[n:]
	}
	for _, v := range []bool{true, false} {
		args := []any{v}
		for id, a := range active {
			if a == v {
				args = append(args, id)
			}
		}
		if len(args) == 1 {
			continue
		}
		if _, err := db.ExecContext(ctx,
			"UPDATE chairs SET is_active = ? WHERE id IN (?"+strings.Repeat(",?", len(args)-2)+")",
			args...); err != nil {
			return err
		}
	}
	return nil
}
