package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

// 通知エンドポイントが毎回DBを引かずに済むよう、ライドの状態をメモリに持つ。
//
// - DBが正。書き込みはこれまでどおりDBに入れ、コミットに成功してからメモリに反映する。
// - 送信済みフラグ(app_sent_at / chair_sent_at)もDBに書く。再起動後に loadState で同じ状態を
//   復元できないと、送信済みの古い状態を再送して「想定外の状態遷移」になるため。
// - 起動時と POST /api/initialize で DB から作り直す。

type statusEntry struct {
	ID        string
	Status    string
	AppSent   bool
	ChairSent bool
}

type rideState struct {
	ID                   string
	UserID               string
	ChairID              string // 未割り当てなら ""
	PickupLatitude       int
	PickupLongitude      int
	DestinationLatitude  int
	DestinationLongitude int
	Fare                 int // クーポン適用後の運賃（作成時に確定する）
	CreatedAt            time.Time
	UpdatedAt            time.Time
	Statuses             []*statusEntry
}

func (r *rideState) latestStatus() string {
	if len(r.Statuses) == 0 {
		return ""
	}
	return r.Statuses[len(r.Statuses)-1].Status
}

type chairInfo struct {
	ID      string
	OwnerID string
	Name    string
	Model   string
}

type chairStatsState struct {
	Count         int
	SumEvaluation int
}

type memState struct {
	mu              sync.Mutex
	rides           map[string]*rideState
	userLatestRide  map[string]*rideState // user_id -> 最新(created_at)のライド
	chairLatestRide map[string]*rideState // chair_id -> 最後に割り当てられたライド
	chairs          map[string]*chairInfo
	chairStats      map[string]*chairStatsState
	userNames       map[string]string // user_id -> "firstname lastname"
}

var st = &memState{}

func loadState(ctx context.Context) error {
	rides := []Ride{}
	if err := db.SelectContext(ctx, &rides, `SELECT * FROM rides ORDER BY created_at`); err != nil {
		return fmt.Errorf("load rides: %w", err)
	}
	type statusRow struct {
		ID        string `db:"id"`
		RideID    string `db:"ride_id"`
		Status    string `db:"status"`
		AppSent   bool   `db:"app_sent"`
		ChairSent bool   `db:"chair_sent"`
	}
	statuses := []statusRow{}
	if err := db.SelectContext(ctx, &statuses, `SELECT id, ride_id, status, app_sent_at IS NOT NULL AS app_sent, chair_sent_at IS NOT NULL AS chair_sent FROM ride_statuses ORDER BY created_at, id`); err != nil {
		return fmt.Errorf("load ride_statuses: %w", err)
	}
	type couponRow struct {
		UsedBy   string `db:"used_by"`
		Discount int    `db:"discount"`
	}
	coupons := []couponRow{}
	if err := db.SelectContext(ctx, &coupons, `SELECT used_by, discount FROM coupons WHERE used_by IS NOT NULL`); err != nil {
		return fmt.Errorf("load coupons: %w", err)
	}
	chairs := []Chair{}
	if err := db.SelectContext(ctx, &chairs, `SELECT * FROM chairs`); err != nil {
		return fmt.Errorf("load chairs: %w", err)
	}
	type userRow struct {
		ID        string `db:"id"`
		Firstname string `db:"firstname"`
		Lastname  string `db:"lastname"`
	}
	users := []userRow{}
	if err := db.SelectContext(ctx, &users, `SELECT id, firstname, lastname FROM users`); err != nil {
		return fmt.Errorf("load users: %w", err)
	}

	discountByRide := make(map[string]int, len(coupons))
	for _, c := range coupons {
		discountByRide[c.UsedBy] = c.Discount
	}

	s := &memState{
		rides:           make(map[string]*rideState, len(rides)),
		userLatestRide:  make(map[string]*rideState),
		chairLatestRide: make(map[string]*rideState),
		chairs:          make(map[string]*chairInfo, len(chairs)),
		chairStats:      make(map[string]*chairStatsState),
		userNames:       make(map[string]string, len(users)),
	}
	for i := range rides {
		rs := newRideState(&rides[i], fareWithDiscount(&rides[i], discountByRide[rides[i].ID]))
		s.rides[rs.ID] = rs
		// created_at 昇順に回しているので、後勝ちで最新になる
		s.userLatestRide[rs.UserID] = rs
	}
	for _, row := range statuses {
		if rs, ok := s.rides[row.RideID]; ok {
			rs.Statuses = append(rs.Statuses, &statusEntry{ID: row.ID, Status: row.Status, AppSent: row.AppSent, ChairSent: row.ChairSent})
		}
	}
	// 椅子の「最新のライド」は元の実装では rides.updated_at DESC。同じ基準で選ぶ。
	for _, rs := range s.rides {
		if rs.ChairID == "" {
			continue
		}
		if cur, ok := s.chairLatestRide[rs.ChairID]; !ok || rs.UpdatedAt.After(cur.UpdatedAt) {
			s.chairLatestRide[rs.ChairID] = rs
		}
	}
	for i := range rides {
		r := &rides[i]
		if r.ChairID.Valid && r.Status == "COMPLETED" && r.Evaluation != nil {
			cs := s.chairStats[r.ChairID.String]
			if cs == nil {
				cs = &chairStatsState{}
				s.chairStats[r.ChairID.String] = cs
			}
			cs.Count++
			cs.SumEvaluation += *r.Evaluation
		}
	}
	for _, c := range chairs {
		s.chairs[c.ID] = &chairInfo{ID: c.ID, OwnerID: c.OwnerID, Name: c.Name, Model: c.Model}
	}
	for _, u := range users {
		s.userNames[u.ID] = fmt.Sprintf("%s %s", u.Firstname, u.Lastname)
	}

	st.mu.Lock()
	st.rides = s.rides
	st.userLatestRide = s.userLatestRide
	st.chairLatestRide = s.chairLatestRide
	st.chairs = s.chairs
	st.chairStats = s.chairStats
	st.userNames = s.userNames
	st.mu.Unlock()
	return nil
}

func fareWithDiscount(r *Ride, discount int) int {
	metered := farePerDistance * calculateDistance(r.PickupLatitude, r.PickupLongitude, r.DestinationLatitude, r.DestinationLongitude)
	return initialFare + max(metered-discount, 0)
}

func newRideState(r *Ride, fare int) *rideState {
	return &rideState{
		ID:                   r.ID,
		UserID:               r.UserID,
		ChairID:              r.ChairID.String,
		PickupLatitude:       r.PickupLatitude,
		PickupLongitude:      r.PickupLongitude,
		DestinationLatitude:  r.DestinationLatitude,
		DestinationLongitude: r.DestinationLongitude,
		Fare:                 fare,
		CreatedAt:            r.CreatedAt,
		UpdatedAt:            r.UpdatedAt,
	}
}

// ロック保持中に呼ぶ。メモリに無いライド（反映前に他の経路が触った等）はDBから読み込む。
func (s *memState) rideLocked(ctx context.Context, rideID string) (*rideState, error) {
	if rs, ok := s.rides[rideID]; ok {
		return rs, nil
	}
	r := Ride{}
	if err := db.GetContext(ctx, &r, `SELECT * FROM rides WHERE id = ?`, rideID); err != nil {
		return nil, err
	}
	discount := 0
	if err := db.GetContext(ctx, &discount, `SELECT discount FROM coupons WHERE used_by = ?`, rideID); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	rs := newRideState(&r, fareWithDiscount(&r, discount))
	rows := []RideStatus{}
	if err := db.SelectContext(ctx, &rows, `SELECT * FROM ride_statuses WHERE ride_id = ? ORDER BY created_at, id`, rideID); err != nil {
		return nil, err
	}
	for _, row := range rows {
		rs.Statuses = append(rs.Statuses, &statusEntry{ID: row.ID, Status: row.Status, AppSent: row.AppSentAt != nil, ChairSent: row.ChairSentAt != nil})
	}
	s.rides[rideID] = rs
	if cur := s.userLatestRide[rs.UserID]; cur == nil || !rs.CreatedAt.Before(cur.CreatedAt) {
		s.userLatestRide[rs.UserID] = rs
	}
	if rs.ChairID != "" {
		if cur := s.chairLatestRide[rs.ChairID]; cur == nil || !rs.UpdatedAt.Before(cur.UpdatedAt) {
			s.chairLatestRide[rs.ChairID] = rs
		}
	}
	return rs, nil
}

// 新しいライド（MATCHING）を作成した。
func (s *memState) addRide(r *Ride, fare int, matchingStatusID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := newRideState(r, fare)
	rs.Statuses = []*statusEntry{{ID: matchingStatusID, Status: "MATCHING"}}
	s.rides[rs.ID] = rs
	s.userLatestRide[rs.UserID] = rs
}

// ライドに椅子を割り当てた。
func (s *memState) assignChair(ctx context.Context, rideID, chairID string, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.rideLocked(ctx, rideID)
	if err != nil {
		return err
	}
	rs.ChairID = chairID
	rs.UpdatedAt = updatedAt
	s.chairLatestRide[chairID] = rs
	return nil
}

// 状態遷移を記録した（ride_statuses にコミット済み）。
func (s *memState) addStatus(ctx context.Context, rideID, statusID, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.rideLocked(ctx, rideID)
	if err != nil {
		return err
	}
	for _, e := range rs.Statuses {
		if e.ID == statusID {
			return nil // DBから読み込んだ時点で既に入っていた
		}
	}
	rs.Statuses = append(rs.Statuses, &statusEntry{ID: statusID, Status: status})
	return nil
}

// ライドが評価されて完了した。
func (s *memState) complete(ctx context.Context, rideID, statusID string, evaluation int, updatedAt time.Time) error {
	if err := s.addStatus(ctx, rideID, statusID, "COMPLETED"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rs := s.rides[rideID]
	rs.UpdatedAt = updatedAt
	cs := s.chairStats[rs.ChairID]
	if cs == nil {
		cs = &chairStatsState{}
		s.chairStats[rs.ChairID] = cs
	}
	cs.Count++
	cs.SumEvaluation += evaluation
	return nil
}

func (s *memState) addChair(c *chairInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chairs[c.ID] = c
}

func (s *memState) addUser(id, firstname, lastname string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userNames[id] = fmt.Sprintf("%s %s", firstname, lastname)
}
