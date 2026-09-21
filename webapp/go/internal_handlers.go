package main

import (
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

type pair struct {
	ride, chair int
	cost        float64
}

func comparePair(a, b pair) int {
	if a.cost != b.cost {
		if a.cost < b.cost {
			return -1
		}
		return 1
	}
	if a.ride != b.ride {
		return a.ride - b.ride
	}
	return a.chair - b.chair
}

type matchingPlan struct {
	RideID  string
	ChairID string
}

var matchingStats struct {
	sync.Mutex
	lastLog  time.Time
	calls    int
	waiting  int
	free     int
	assigned int
}

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる。
//
// 待っているライドと空いている椅子の組を、迎車時間(椅子→乗車位置 / speed)が短い順に割り当てる
// （待ち時間が長いライドを優先する補正つき）。
// 判定はすべてメモリ上の状態で行い、DBには割り当て結果だけを書く。
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	type freeChair struct {
		ID          string
		Speed       int
		HasLocation bool
		Latitude    int
		Longitude   int
	}

	st.mu.Lock()
	waiting := make([]*rideState, 0, len(st.waitingRides))
	for _, rs := range st.waitingRides {
		if rs.ChairID == "" && rs.latestStatus() == "MATCHING" {
			waiting = append(waiting, rs)
		}
	}
	chairs := make([]freeChair, 0)
	if len(waiting) > 0 {
		for _, c := range st.freeChairs {
			// 空いている椅子 = 稼働中・前のライドの COMPLETED を椅子に通知済み（+ 猶予）
			if !c.IsActive {
				continue
			}
			if rs := st.chairLatestRide[c.ID]; rs != nil && !rs.releasedChair() {
				continue
			}
			chairs = append(chairs, freeChair{ID: c.ID, Speed: st.chairSpeed(c.Model), HasLocation: c.HasLocation, Latitude: c.Latitude, Longitude: c.Longitude})
		}
	}
	type rideView struct {
		ID                                     string
		CreatedAt                              time.Time
		PickupLat, PickupLon, DestLat, DestLon int
	}
	rides := make([]rideView, 0, len(waiting))
	for _, rs := range waiting {
		rides = append(rides, rideView{rs.ID, rs.CreatedAt, rs.PickupLatitude, rs.PickupLongitude, rs.DestinationLatitude, rs.DestinationLongitude})
	}
	st.mu.Unlock()

	slices.SortFunc(rides, func(a, b rideView) int { return a.CreatedAt.Compare(b.CreatedAt) })
	// 同点のときに結果が毎回ぶれないよう、IDで並べておく
	slices.SortFunc(chairs, func(a, b freeChair) int { return strings.Compare(a.ID, b.ID) })

	// すべての（ライド, 空き椅子）の組を「乗車位置に着くまでの時間」が短い順に貪欲に割り当てる。
	// 椅子が足りない（待ちライド >> 空き椅子）ときに古いライドから順に選ぶと、古いライドが
	// 別のライドのすぐ近くにいた椅子を遠くから取っていき、迎車の距離（=椅子の空走時間）が伸びる。
	// 取り残し防止に、待ち時間が長いライドほどコストを下げる（1秒待つごとに agingPerSec ぶん）。
	const agingPerSec = 4.0
	now := time.Now()
	// 各椅子について、コストの小さいライドを len(chairs) 件だけ候補に残す。
	// 貪欲法で椅子 c が割り当てられるまでに他の椅子に取られるライドは高々 len(chairs)-1 件なので、
	// 全組を並べたときと結果は変わらない（待ちライドが数千・空き椅子が数十のとき、全組ソートが
	// アプリの CPU の 34% を使っていた）。
	ages := make([]float64, len(rides))
	for ri, ride := range rides {
		ages[ri] = now.Sub(ride.CreatedAt).Seconds()
	}
	// 乗車時間の項: 長いライドほど速い椅子に当てる（運搬時間の合計が減り、椅子が早く空く）。
	// 乗車距離をそのまま足すと長いライドが後回しになる（どの依頼から配るかが変わる: 前回 -6%）ので、
	// この回の空き椅子の速さの調和平均 vRef で運んだときの時間を引き、椅子全体で平均すると 0 になる形にする。
	const rideTimeWeight = 1.0
	vRef := 0.0
	if len(chairs) > 0 {
		inv := 0.0
		for _, c := range chairs {
			inv += 1 / float64(c.Speed)
		}
		vRef = float64(len(chairs)) / inv
	}
	rideDist := make([]float64, len(rides))
	for ri, ride := range rides {
		rideDist[ri] = float64(calculateDistance(ride.PickupLat, ride.PickupLon, ride.DestLat, ride.DestLon))
	}
	k := len(chairs)
	pairs := make([]pair, 0, len(chairs)*min(k, len(rides)))
	cand := make([]pair, 0, len(rides))
	for ci, c := range chairs {
		cand = cand[:0]
		for ri, ride := range rides {
			// 位置が一度も送られていない椅子は、どこにいるか分からないので最後の手段にする
			pickupDistance := 1 << 20
			if c.HasLocation {
				pickupDistance = calculateDistance(c.Latitude, c.Longitude, ride.PickupLat, ride.PickupLon)
			}
			cost := float64(pickupDistance)/float64(c.Speed) +
				rideTimeWeight*rideDist[ri]*(1/float64(c.Speed)-1/vRef) -
				agingPerSec*ages[ri]
			cand = append(cand, pair{ri, ci, cost})
		}
		if len(cand) > k {
			// 上位 k 件だけ欲しいので部分的に並べる（k は小さい）
			slices.SortFunc(cand, comparePair)
			cand = cand[:k]
		}
		pairs = append(pairs, cand...)
	}
	slices.SortFunc(pairs, comparePair)
	plans := make([]matchingPlan, 0)
	usedRide := make([]bool, len(rides))
	usedChair := make([]bool, len(chairs))
	for _, p := range pairs {
		if usedRide[p.ride] || usedChair[p.chair] {
			continue
		}
		usedRide[p.ride], usedChair[p.chair] = true, true
		plans = append(plans, matchingPlan{RideID: rides[p.ride].ID, ChairID: chairs[p.chair].ID})
	}

	// 割り当ては「メモリ → DB」の順に反映する。
	// DBを先にすると、UPDATE の数msの間に nearby-chairs がその椅子を空きとして返し、
	// retrieved_at がマッチ時刻(updated_at)より後になって「既にライド中」の WARN になった。
	// DB へは UPDATE 1文にまとめる（1件ずつだと 150件で約2.5秒かかっていた）。
	// マッチングは matcher から直列に呼ばれるので、chair_id IS NULL の競合は起きない。
	assigned := 0
	if len(plans) > 0 {
		now, err := st.assignChairs(ctx, plans)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		assigned = len(plans)
		query := "UPDATE rides SET updated_at = ?, chair_id = CASE id"
		args := make([]any, 0, len(plans)*3+1)
		args = append(args, now)
		for _, p := range plans {
			query += " WHEN ? THEN ?"
			args = append(args, p.RideID, p.ChairID)
		}
		query += " END WHERE id IN (?" + strings.Repeat(",?", len(plans)-1) + ") AND chair_id IS NULL"
		for _, p := range plans {
			args = append(args, p.RideID)
		}
		_, err = db.ExecContext(ctx, query, args...)
		// 失敗しても椅子を止めたままにはしない（ログに残して通知は進める）
		st.finishAssign(plans)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
	}

	// 計測: 待ちライド数・空き椅子数・割り当て数を5秒ごとに1行だけ出す
	matchingStats.Lock()
	matchingStats.calls++
	matchingStats.waiting += len(rides)
	matchingStats.free += len(chairs)
	matchingStats.assigned += assigned
	if time.Since(matchingStats.lastLog) >= 5*time.Second {
		if matchingStats.calls > 0 {
			slog.Info("matching",
				"calls", matchingStats.calls,
				"avg_waiting", float64(matchingStats.waiting)/float64(matchingStats.calls),
				"avg_free_chairs", float64(matchingStats.free)/float64(matchingStats.calls),
				"assigned", matchingStats.assigned)
		}
		matchingStats.lastLog = time.Now()
		matchingStats.calls, matchingStats.waiting, matchingStats.free, matchingStats.assigned = 0, 0, 0, 0
	}
	matchingStats.Unlock()

	w.WriteHeader(http.StatusNoContent)
}
