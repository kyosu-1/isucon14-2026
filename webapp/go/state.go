package main

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// 通知エンドポイントが毎回DBを引かずに済むよう、ライドの状態をメモリに持つ。
//
// - DBが正。書き込みはこれまでどおりDBに入れ、コミットに成功してからメモリに反映する。
// - 送信済みフラグ(app_sent_at / chair_sent_at)はメモリだけに持つ。追試は「再起動 → initialize → 負荷」
//   なので、負荷中の送信済みを DB に残しても使われない（初期データの値は loadState で読む）。
// - 起動時と POST /api/initialize で DB から作り直す。

type statusEntry struct {
	ID          string
	Status      string
	AppSent     bool
	ChairSent   bool
	ChairSentAt time.Time // メモリ上で送信済みにした時刻（DBから復元したものはゼロ値）
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
	Evaluation           int // 完了したライドの評価
	CreatedAt            time.Time
	UpdatedAt            time.Time
	Statuses             []*statusEntry
	// マッチングでメモリには割り当てたが、DBへの UPDATE がまだコミットされていない。
	// この間は椅子に通知しない（椅子が先に進むと、DB の chair_id が空のまま状態遷移・評価が起き、
	// 遅れて来たマッチングの UPDATE が updated_at（完了日時）を上書きした）。
	AssignPending bool
	// 評価の処理中（決済〜DB書き込み）。同じライドへの評価の二重処理を防ぐ
	Completing bool
}

// 椅子がこのライドから解放されたか = 最新が COMPLETED で、それを椅子に通知済み。
// さらに通知してから releaseGrace 経つまでは解放しない（応答が椅子に届いて処理されるまでの余裕）。
// DB上で COMPLETED になっても、椅子が完了通知を受け取るまでは「ライド中」として扱う
// （マッチングの空き判定と同じ基準。nearby-chairs でこれより早く出すと「既にライド中」の WARN になった）。
func (r *rideState) releasedChair() bool {
	return r.releasedChairFor(releaseGrace)
}

// COMPLETED を椅子に通知してから grace 以上経っているか
func (r *rideState) releasedChairFor(grace time.Duration) bool {
	if len(r.Statuses) == 0 {
		return false
	}
	last := r.Statuses[len(r.Statuses)-1]
	return last.Status == "COMPLETED" && last.ChairSent && time.Since(last.ChairSentAt) >= grace
}

const releaseGrace = 50 * time.Millisecond

// nearby-chairs に出すまでの猶予。ベンチ側は COMPLETED から ~100ms 経っても「ライド中」と見ていることがある
// （measurements/20260922-032055: COMPLETED 53.385 に対し 53.486〜.515 の nearby 7件が WARN）。
// マッチング間隔 0.1s の今は、解放された椅子はもともと ~100ms 以内に割り当てられるので見える時間は短い。
const nearbyReleaseGrace = 300 * time.Millisecond

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

	CreatedAt   time.Time
	IsActive    bool
	HasLocation bool // 一度でも座標を送ってきたか
	Latitude    int
	Longitude   int

	TotalDistance          int
	TotalDistanceUpdatedAt time.Time
}

type chairStatsState struct {
	Count         int
	SumEvaluation int
	// 完了したライドの売上（割引前の運賃）と完了日時。owner/sales をメモリで計算する
	Sales []saleEntry
}

type saleEntry struct {
	At   time.Time
	Sale int
}

type memState struct {
	mu              sync.Mutex
	rides           map[string]*rideState
	userLatestRide  map[string]*rideState   // user_id -> 最新(created_at)のライド
	userRides       map[string][]*rideState // user_id -> ライド（作成順）
	ownerNames      map[string]string       // owner_id -> オーナー名
	chairLatestRide map[string]*rideState   // chair_id -> 最後に割り当てられたライド
	chairs          map[string]*chairInfo
	chairStats      map[string]*chairStatsState
	userNames       map[string]string        // user_id -> "firstname lastname"
	modelSpeed      map[string]int           // chair_models: モデル名 -> speed（マスタデータ）
	dirtyChairs     map[string]struct{}      // 移動距離をまだDBに書き出していない椅子
	paymentTokens   map[string]string        // user_id -> 決済トークン
	paymentURL      string                   // 決済サーバーのURL（initialize で設定される）
	inviteUsed      map[string]int           // "INV_<招待コード>" -> 使われた回数（上限3）
	userWake        map[string]chan struct{} // SSE: 利用者の通知ストリームを起こす
	chairWake       map[string]chan struct{} // SSE: 椅子の通知ストリームを起こす
}

// ロック保持中に呼ぶ
func (s *memState) chairSpeed(model string) int {
	if v, ok := s.modelSpeed[model]; ok && v > 0 {
		return v
	}
	return 1
}

var st = &memState{}

// アクセストークン -> 利用者/椅子/オーナー。トークンは発行後に変わらないので、
// 認証のたびにDBを引かない。起動時と initialize で作り直し、登録APIで追加する。
// （Chair.IsActive などトークン以外の可変な列は認証結果として使わないこと）
type authCacheT struct {
	mu     sync.RWMutex
	users  map[string]*User
	chairs map[string]*Chair
	owners map[string]*Owner
}

var authCache = &authCacheT{}

func (a *authCacheT) user(token string) (*User, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	u, ok := a.users[token]
	return u, ok
}

func (a *authCacheT) chair(token string) (*Chair, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	c, ok := a.chairs[token]
	return c, ok
}

func (a *authCacheT) owner(token string) (*Owner, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	o, ok := a.owners[token]
	return o, ok
}

func (a *authCacheT) putUser(u *User) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.users[u.AccessToken] = u
}

func (a *authCacheT) putChair(c *Chair) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.chairs[c.AccessToken] = c
}

func (a *authCacheT) putOwner(o *Owner) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.owners[o.AccessToken] = o
}

func loadAuthCache(ctx context.Context) error {
	users := []User{}
	if err := db.SelectContext(ctx, &users, `SELECT * FROM users`); err != nil {
		return fmt.Errorf("load users: %w", err)
	}
	chairs := []Chair{}
	if err := db.SelectContext(ctx, &chairs, `SELECT * FROM chairs`); err != nil {
		return fmt.Errorf("load chairs: %w", err)
	}
	owners := []Owner{}
	if err := db.SelectContext(ctx, &owners, `SELECT * FROM owners`); err != nil {
		return fmt.Errorf("load owners: %w", err)
	}
	um := make(map[string]*User, len(users))
	for i := range users {
		um[users[i].AccessToken] = &users[i]
	}
	cm := make(map[string]*Chair, len(chairs))
	for i := range chairs {
		cm[chairs[i].AccessToken] = &chairs[i]
	}
	om := make(map[string]*Owner, len(owners))
	for i := range owners {
		om[owners[i].AccessToken] = &owners[i]
	}
	authCache.mu.Lock()
	authCache.users, authCache.chairs, authCache.owners = um, cm, om
	authCache.mu.Unlock()
	return nil
}

func loadState(ctx context.Context) error {
	if err := loadAuthCache(ctx); err != nil {
		return err
	}
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
	type locRow struct {
		ChairID                string    `db:"chair_id"`
		TotalDistance          int       `db:"total_distance"`
		TotalDistanceUpdatedAt time.Time `db:"total_distance_updated_at"`
		Latitude               int       `db:"latitude"`
		Longitude              int       `db:"longitude"`
	}
	locs := []locRow{}
	if err := db.SelectContext(ctx, &locs, `SELECT chair_id, total_distance, total_distance_updated_at, latitude, longitude FROM chair_distances`); err != nil {
		return fmt.Errorf("load chair_distances: %w", err)
	}

	tokens := []PaymentToken{}
	if err := db.SelectContext(ctx, &tokens, `SELECT * FROM payment_tokens`); err != nil {
		return fmt.Errorf("load payment_tokens: %w", err)
	}
	paymentURL := ""
	if err := db.GetContext(ctx, &paymentURL, "SELECT value FROM settings WHERE name = 'payment_gateway_url'"); err != nil {
		return fmt.Errorf("load settings: %w", err)
	}
	models := []ChairModel{}
	if err := db.SelectContext(ctx, &models, `SELECT * FROM chair_models`); err != nil {
		return fmt.Errorf("load chair_models: %w", err)
	}
	type inviteRow struct {
		Code  string `db:"code"`
		Count int    `db:"cnt"`
	}
	invites := []inviteRow{}
	if err := db.SelectContext(ctx, &invites, `SELECT code, COUNT(*) AS cnt FROM coupons WHERE code LIKE 'INV\_%' GROUP BY code`); err != nil {
		return fmt.Errorf("load invitations: %w", err)
	}

	discountByRide := make(map[string]int, len(coupons))
	for _, c := range coupons {
		discountByRide[c.UsedBy] = c.Discount
	}

	s := &memState{
		rides:           make(map[string]*rideState, len(rides)),
		userLatestRide:  make(map[string]*rideState),
		userRides:       make(map[string][]*rideState),
		ownerNames:      make(map[string]string),
		chairLatestRide: make(map[string]*rideState),
		chairs:          make(map[string]*chairInfo, len(chairs)),
		chairStats:      make(map[string]*chairStatsState),
		userNames:       make(map[string]string, len(users)),
		modelSpeed:      make(map[string]int, len(models)),
	}
	for _, m := range models {
		s.modelSpeed[m.Name] = m.Speed
	}
	for i := range rides {
		rs := newRideState(&rides[i], fareWithDiscount(&rides[i], discountByRide[rides[i].ID]))
		s.rides[rs.ID] = rs
		// created_at 昇順に回しているので、後勝ちで最新になる
		s.userLatestRide[rs.UserID] = rs
		s.userRides[rs.UserID] = append(s.userRides[rs.UserID], rs)
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
			cs.Sales = append(cs.Sales, saleEntry{At: r.UpdatedAt, Sale: calculateSale(*r)})
		}
	}
	for _, c := range chairs {
		s.chairs[c.ID] = &chairInfo{ID: c.ID, OwnerID: c.OwnerID, Name: c.Name, Model: c.Model, CreatedAt: c.CreatedAt, IsActive: c.IsActive}
	}
	for _, l := range locs {
		if c := s.chairs[l.ChairID]; c != nil {
			c.HasLocation, c.Latitude, c.Longitude = true, l.Latitude, l.Longitude
			c.TotalDistance, c.TotalDistanceUpdatedAt = l.TotalDistance, l.TotalDistanceUpdatedAt
		}
	}
	for _, u := range users {
		s.userNames[u.ID] = fmt.Sprintf("%s %s", u.Firstname, u.Lastname)
	}
	owners := []Owner{}
	if err := db.SelectContext(ctx, &owners, `SELECT * FROM owners`); err != nil {
		return fmt.Errorf("load owners: %w", err)
	}
	for _, o := range owners {
		s.ownerNames[o.ID] = o.Name
	}

	st.mu.Lock()
	st.rides = s.rides
	st.userLatestRide = s.userLatestRide
	st.userRides = s.userRides
	st.ownerNames = s.ownerNames
	st.chairLatestRide = s.chairLatestRide
	st.chairs = s.chairs
	st.chairStats = s.chairStats
	st.userNames = s.userNames
	st.modelSpeed = s.modelSpeed
	st.dirtyChairs = make(map[string]struct{})
	st.paymentTokens = make(map[string]string, len(tokens))
	for _, t := range tokens {
		st.paymentTokens[t.UserID] = t.Token
	}
	st.paymentURL = paymentURL
	st.inviteUsed = make(map[string]int, len(invites))
	for _, iv := range invites {
		st.inviteUsed[iv.Code] = iv.Count
	}
	if st.userWake == nil {
		st.userWake = make(map[string]chan struct{})
		st.chairWake = make(map[string]chan struct{})
	}
	st.mu.Unlock()
	return nil
}

func fareWithDiscount(r *Ride, discount int) int {
	metered := farePerDistance * calculateDistance(r.PickupLatitude, r.PickupLongitude, r.DestinationLatitude, r.DestinationLongitude)
	return initialFare + max(metered-discount, 0)
}

func newRideState(r *Ride, fare int) *rideState {
	eval := 0
	if r.Evaluation != nil {
		eval = *r.Evaluation
	}
	return &rideState{
		Evaluation:           eval,
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

// ロック保持中に呼ぶ。SSE のストリーム1本ぶんのチャネルを作って登録する（容量1。取りこぼしても次の判定で拾える）。
// 同じ利用者・椅子が張り直したら、新しい接続のチャネルで置き換える。古い接続のハンドラは
// isCurrent が false になった時点で何も送らずに終わる。以前は1本のチャネルを共有していたので、
// 張り直しの瞬間に古い（切れかけの）接続が起こされて状態を「送信済み」にし、新しい接続に届かなかった
// （同じ椅子で nearby の「既にライド中」が数秒続いた）。
func subscribe(m map[string]chan struct{}, key string) chan struct{} {
	ch := make(chan struct{}, 1)
	m[key] = ch
	return ch
}

// ロック保持中に呼ぶ
func isCurrent(m map[string]chan struct{}, key string, ch chan struct{}) bool {
	return m[key] == ch
}

// ロック保持中に呼ぶ
func wake(m map[string]chan struct{}, key string) {
	if key == "" {
		return
	}
	if ch, ok := m[key]; ok {
		select {
		case ch <- struct{}{}:
		default:
		}
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
	s.userRides[rs.UserID] = append(s.userRides[rs.UserID], rs)
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
	s.userRides[rs.UserID] = append(s.userRides[rs.UserID], rs)
	wake(s.userWake, rs.UserID)
}

// マッチング結果をまとめてメモリに反映し、割り当て時刻を返す。
// 時刻はロックの中で決める。nearby-chairs の retrieved_at もロックの中で取るので、
// 「retrieved_at より前にマッチした椅子を空きとして返す」ことが起きない。
func (s *memState) assignChairs(ctx context.Context, plans []matchingPlan) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Truncate(time.Microsecond)
	for _, p := range plans {
		rs, err := s.rideLocked(ctx, p.RideID)
		if err != nil {
			return now, err
		}
		rs.ChairID = p.ChairID
		rs.UpdatedAt = now
		rs.AssignPending = true
		s.chairLatestRide[p.ChairID] = rs
	}
	return now, nil
}

// マッチング結果が DB にコミットされたので、椅子に通知してよい
func (s *memState) finishAssign(plans []matchingPlan) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range plans {
		if rs, ok := s.rides[p.RideID]; ok {
			rs.AssignPending = false
		}
		wake(s.chairWake, p.ChairID)
	}
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
	wake(s.userWake, rs.UserID)
	wake(s.chairWake, rs.ChairID)
	return nil
}

// ライドが評価されて完了した。
// 椅子の統計を先に更新してから COMPLETED を積む（同じロックの中で）。
// COMPLETED を積んだ瞬間に SSE が利用者へ送るので、統計が後だと
// 「椅子の総乗車回数が一致しません」の WARN になった。
func (s *memState) complete(ctx context.Context, rideID, statusID string, evaluation int, updatedAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.rideLocked(ctx, rideID)
	if err != nil {
		return err
	}
	for _, e := range rs.Statuses {
		if e.ID == statusID {
			return nil // DBから読み込んだ時点で反映済み（統計も loadState / rideLocked 側の扱い）
		}
	}
	rs.UpdatedAt = updatedAt
	rs.Evaluation = evaluation
	cs := s.chairStats[rs.ChairID]
	if cs == nil {
		cs = &chairStatsState{}
		s.chairStats[rs.ChairID] = cs
	}
	cs.Count++
	cs.SumEvaluation += evaluation
	cs.Sales = append(cs.Sales, saleEntry{At: updatedAt, Sale: calculateFare(rs.PickupLatitude, rs.PickupLongitude, rs.DestinationLatitude, rs.DestinationLongitude)})
	rs.Statuses = append(rs.Statuses, &statusEntry{ID: statusID, Status: "COMPLETED"})
	wake(s.userWake, rs.UserID)
	wake(s.chairWake, rs.ChairID)
	return nil
}

// 招待コードの枠を1つ予約する（上限3回）。返り値は何番目の枠か。
func (s *memState) reserveInvitation(code string) (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inviteUsed[code] >= 3 {
		return 0, false
	}
	s.inviteUsed[code]++
	return s.inviteUsed[code], true
}

// 登録が失敗したときに予約した枠を返す
func (s *memState) releaseInvitation(code string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.inviteUsed[code] > 0 {
		s.inviteUsed[code]--
	}
}

func (s *memState) setPaymentToken(userID, token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paymentTokens[userID] = token
}

func (s *memState) addOwner(id, name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ownerNames[id] = name
}

func (s *memState) addChair(c *chairInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.chairs[c.ID] = c
}

func (s *memState) setChairActive(chairID string, active bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.chairs[chairID]; c != nil {
		c.IsActive = active
	}
}

// 座標を記録する。移動距離の合計（owner/chairs 用）もここで積み上げ、DBへは flushChairDistances がまとめて書く。
func (s *memState) setChairLocation(chairID string, lat, lon int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.chairs[chairID]
	if c == nil {
		return
	}
	if c.HasLocation {
		c.TotalDistance += abs(c.Latitude-lat) + abs(c.Longitude-lon)
	}
	c.HasLocation, c.Latitude, c.Longitude = true, lat, lon
	c.TotalDistanceUpdatedAt = now
	s.dirtyChairs[chairID] = struct{}{}
}

// flushMu: 書き出しと initialize を排他する。
// initialize がテーブルを作り直した後に、前回のベンチの距離を書き戻さないため。
var flushMu sync.Mutex

func startFlusher() {
	go func() {
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		for range t.C {
			if err := flushChairDistances(context.Background()); err != nil {
				slog.Error("flush chair_distances", "err", err)
			}
		}
	}()
}

// メモリ上の移動距離・最新座標を chair_distances にまとめて書く（再起動後の復元用）。
func flushChairDistances(ctx context.Context) error {
	flushMu.Lock()
	defer flushMu.Unlock()

	type row struct {
		id        string
		total     int
		updatedAt time.Time
		lat, lon  int
	}
	st.mu.Lock()
	rows := make([]row, 0, len(st.dirtyChairs))
	for id := range st.dirtyChairs {
		if c := st.chairs[id]; c != nil {
			rows = append(rows, row{id, c.TotalDistance, c.TotalDistanceUpdatedAt, c.Latitude, c.Longitude})
		}
	}
	st.dirtyChairs = make(map[string]struct{})
	st.mu.Unlock()

	for len(rows) > 0 {
		n := min(len(rows), 500)
		chunk := rows[:n]
		rows = rows[n:]
		query := "INSERT INTO chair_distances (chair_id, total_distance, total_distance_updated_at, latitude, longitude) VALUES " +
			strings.TrimSuffix(strings.Repeat("(?, ?, ?, ?, ?),", len(chunk)), ",") +
			" AS new ON DUPLICATE KEY UPDATE total_distance = new.total_distance, total_distance_updated_at = new.total_distance_updated_at, latitude = new.latitude, longitude = new.longitude"
		args := make([]any, 0, len(chunk)*5)
		for _, r := range chunk {
			args = append(args, r.id, r.total, r.updatedAt, r.lat, r.lon)
		}
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

func (s *memState) addUser(id, firstname, lastname string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.userNames[id] = fmt.Sprintf("%s %s", firstname, lastname)
}
