package main

import (
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

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
	waiting := make([]*rideState, 0)
	for _, rs := range st.rides {
		if rs.ChairID == "" && rs.latestStatus() == "MATCHING" {
			waiting = append(waiting, rs)
		}
	}
	chairs := make([]freeChair, 0)
	if len(waiting) > 0 {
		for _, c := range st.chairs {
			// 空いている椅子 = 稼働中・前のライドの COMPLETED を椅子に通知済み
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

	sort.Slice(rides, func(i, j int) bool { return rides[i].CreatedAt.Before(rides[j].CreatedAt) })
	// 同点のときに結果が毎回ぶれないよう、IDで並べておく
	sort.Slice(chairs, func(i, j int) bool { return chairs[i].ID < chairs[j].ID })

	// すべての（ライド, 空き椅子）の組を「乗車位置に着くまでの時間」が短い順に貪欲に割り当てる。
	// 椅子が足りない（待ちライド >> 空き椅子）ときに古いライドから順に選ぶと、古いライドが
	// 別のライドのすぐ近くにいた椅子を遠くから取っていき、迎車の距離（=椅子の空走時間）が伸びる。
	// 取り残し防止に、待ち時間が長いライドほどコストを下げる（1秒待つごとに agingPerSec ぶん）。
	const agingPerSec = 2.0
	now := time.Now()
	type pair struct {
		ride, chair int
		cost        float64
	}
	pairs := make([]pair, 0, len(rides)*len(chairs))
	for ri, ride := range rides {
		age := now.Sub(ride.CreatedAt).Seconds()
		for ci, c := range chairs {
			// 位置が一度も送られていない椅子は、どこにいるか分からないので最後の手段にする
			pickupDistance := 1 << 20
			if c.HasLocation {
				pickupDistance = calculateDistance(c.Latitude, c.Longitude, ride.PickupLat, ride.PickupLon)
			}
			cost := float64(pickupDistance)/float64(c.Speed) - agingPerSec*age
			pairs = append(pairs, pair{ri, ci, cost})
		}
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].cost != pairs[j].cost {
			return pairs[i].cost < pairs[j].cost
		}
		if pairs[i].ride != pairs[j].ride {
			return pairs[i].ride < pairs[j].ride
		}
		return pairs[i].chair < pairs[j].chair
	})
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
		if _, err := db.ExecContext(ctx, query, args...); err != nil {
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
