package main

import (
	"log/slog"
	"net/http"
	"sort"
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
// 待っているライドを古い順に、空いている椅子のうち「乗車位置に着いて目的地まで運び終えるまでの時間」
// (= (椅子→乗車位置 + 乗車位置→目的地) / speed) が最短のものへ割り当てる。
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

	plans := make([]matchingPlan, 0)
	used := make([]bool, len(chairs))
	for _, ride := range rides {
		best := -1
		var bestCost float64
		rideDistance := calculateDistance(ride.PickupLat, ride.PickupLon, ride.DestLat, ride.DestLon)
		for i, c := range chairs {
			if used[i] {
				continue
			}
			// 位置が一度も送られていない椅子は、どこにいるか分からないので最後の手段にする
			pickupDistance := 1 << 20
			if c.HasLocation {
				pickupDistance = calculateDistance(c.Latitude, c.Longitude, ride.PickupLat, ride.PickupLon)
			}
			cost := float64(pickupDistance+rideDistance) / float64(c.Speed)
			if best < 0 || cost < bestCost {
				best, bestCost = i, cost
			}
		}
		if best < 0 {
			break
		}
		used[best] = true
		plans = append(plans, matchingPlan{RideID: ride.ID, ChairID: chairs[best].ID})
	}

	assigned := 0
	for _, p := range plans {
		// updated_at はメモリにも同じ値を持ちたいので、DBの ON UPDATE に任せず明示する
		now := time.Now().UTC().Truncate(time.Microsecond)
		res, err := db.ExecContext(ctx, "UPDATE rides SET chair_id = ?, updated_at = ? WHERE id = ? AND chair_id IS NULL", p.ChairID, now, p.RideID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if n, _ := res.RowsAffected(); n == 1 {
			if err := st.assignChair(ctx, p.RideID, p.ChairID, now); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			assigned++
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
