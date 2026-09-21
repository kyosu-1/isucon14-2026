package main

import (
	"log/slog"
	"math"
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
	lastLog   time.Time
	calls     int
	waiting   int
	free      int
	assigned  int
	hungarian int
	maxDur    time.Duration
	maxLock   time.Duration // 状態のロックを取るまで
	maxCalc   time.Duration // 割り当ての計算
	maxDB     time.Duration // メモリ反映 + DB の UPDATE
}

// 椅子 0..nChairs-1 と候補ライド candRides の間で、コストの合計が最小になる割り当てを返す（[椅子, ライド] の組）。
// 数の少ない側を行にしてハンガリアン法を解く（少ない側はすべて割り当てられる）。
func minCostAssign(nChairs int, candRides []int, costOf func(ci, ri int) float64) [][2]int {
	rowsAreChairs := nChairs <= len(candRides)
	nr, nc := nChairs, len(candRides)
	if !rowsAreChairs {
		nr, nc = nc, nr
	}
	if nr == 0 {
		return nil
	}
	a := make([][]float64, nr)
	for i := range a {
		a[i] = make([]float64, nc)
		for j := range a[i] {
			if rowsAreChairs {
				a[i][j] = costOf(i, candRides[j])
			} else {
				a[i][j] = costOf(j, candRides[i])
			}
		}
	}
	out := make([][2]int, 0, nr)
	for i, j := range hungarian(a) {
		if rowsAreChairs {
			out = append(out, [2]int{i, candRides[j]})
		} else {
			out = append(out, [2]int{j, candRides[i]})
		}
	}
	return out
}

// ハンガリアン法を使う計算量の上限（行² × 列）。超える回は貪欲法
const hungarianBudget = 20_000_000

// 最小コストの割り当て（Kuhn-Munkres、O(n²m)）。a は n×m（n <= m）のコスト行列。
// 返り値 assign[i] は行 i に割り当てた列。
func hungarian(a [][]float64) []int {
	n := len(a)
	if n == 0 {
		return nil
	}
	m := len(a[0])
	inf := math.Inf(1)
	u := make([]float64, n+1)
	v := make([]float64, m+1)
	p := make([]int, m+1)
	way := make([]int, m+1)
	minv := make([]float64, m+1)
	used := make([]bool, m+1)
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		for j := 0; j <= m; j++ {
			minv[j] = inf
			used[j] = false
		}
		for {
			used[j0] = true
			i0 := p[j0]
			delta := inf
			j1 := 0
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				cur := a[i0-1][j-1] - u[i0] - v[j]
				if cur < minv[j] {
					minv[j] = cur
					way[j] = j0
				}
				if minv[j] < delta {
					delta = minv[j]
					j1 = j
				}
			}
			for j := 0; j <= m; j++ {
				if used[j] {
					u[p[j]] += delta
					v[j] -= delta
				} else {
					minv[j] -= delta
				}
			}
			j0 = j1
			if p[j0] == 0 {
				break
			}
		}
		for {
			j1 := way[j0]
			p[j0] = p[j1]
			j0 = j1
			if j0 == 0 {
				break
			}
		}
	}
	assign := make([]int, n)
	for j := 1; j <= m; j++ {
		if p[j] != 0 {
			assign[p[j]-1] = j - 1
		}
	}
	return assign
}

// このAPIをインスタンス内から一定間隔で叩かせることで、椅子とライドをマッチングさせる。
//
// 待っているライドと空いている椅子の組を、迎車時間(椅子→乗車位置 / speed)が短い順に割り当てる
// （待ち時間が長いライドを優先する補正つき）。
// 判定はすべてメモリ上の状態で行い、DBには割り当て結果だけを書く。
func internalGetMatching(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	started := time.Now()
	usedHungarian := false
	var tLock, tCalc, tDB time.Duration

	type freeChair struct {
		ID          string
		Speed       int
		HasLocation bool
		Latitude    int
		Longitude   int
	}

	st.mu.Lock()
	tLock = time.Since(started)
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
	const agingPerSec = 8.0
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
	costOf := func(ci, ri int) float64 {
		c, ride := chairs[ci], rides[ri]
		// 位置が一度も送られていない椅子は、どこにいるか分からないので最後の手段にする
		pickupDistance := 1 << 20
		if c.HasLocation {
			pickupDistance = calculateDistance(c.Latitude, c.Longitude, ride.PickupLat, ride.PickupLon)
		}
		return float64(pickupDistance)/float64(c.Speed) +
			rideTimeWeight*rideDist[ri]*(1/float64(c.Speed)-1/vRef) -
			agingPerSec*ages[ri]
	}
	k := len(chairs)
	pairs := make([]pair, 0, len(chairs)*min(k, len(rides)))
	cand := make([]pair, 0, len(rides))
	for ci := range chairs {
		cand = cand[:0]
		for ri := range rides {
			cand = append(cand, pair{ri, ci, costOf(ci, ri)})
		}
		if len(cand) > k {
			// 上位 k 件だけ欲しいので部分的に並べる（k は小さい）
			slices.SortFunc(cand, comparePair)
			cand = cand[:k]
		}
		pairs = append(pairs, cand...)
	}
	plans := make([]matchingPlan, 0)
	// その回の割り当て全体でコストの合計を最小にする（ハンガリアン法）。
	// 候補ライドは上の絞り込み（各椅子の上位 len(chairs) 件）の和集合。計算量 O(行² × 列) が大きすぎる
	// 回（負荷の立ち上がりで空き椅子が数百脚ある等）は、従来の貪欲法にする。
	candRides := make([]int, 0)
	seen := make(map[int]bool)
	for _, p := range pairs {
		if !seen[p.ride] {
			seen[p.ride] = true
			candRides = append(candRides, p.ride)
		}
	}
	slices.Sort(candRides)
	nr, nc := len(chairs), len(candRides)
	if nr > nc {
		nr, nc = nc, nr
	}
	usedHungarian = nr > 0 && nr*nr*nc <= hungarianBudget
	if usedHungarian {
		for _, cr := range minCostAssign(len(chairs), candRides, costOf) {
			plans = append(plans, matchingPlan{RideID: rides[cr[1]].ID, ChairID: chairs[cr[0]].ID})
		}
	} else {
		slices.SortFunc(pairs, comparePair)
		usedRide := make([]bool, len(rides))
		usedChair := make([]bool, len(chairs))
		for _, p := range pairs {
			if usedRide[p.ride] || usedChair[p.chair] {
				continue
			}
			usedRide[p.ride], usedChair[p.chair] = true, true
			plans = append(plans, matchingPlan{RideID: rides[p.ride].ID, ChairID: chairs[p.chair].ID})
		}
	}

	tCalc = time.Since(started) - tLock
	dbStart := time.Now()
	// 割り当てはメモリに反映して、すぐ椅子に通知する。DB へは状態遷移と同じ FIFO の writer が書く。
	// 以前は UPDATE のコミットを待ってから通知していた（ピーク時に1回 最大 380ms、その間 椅子が待たされた）。
	// FIFO なので、割り当ての書き込みは同じライドのこれ以降の状態遷移・評価より必ず先に DB に入る。
	// 割り当て時刻はロックの中で決める（nearby-chairs の retrieved_at と前後関係をそろえる）。
	assigned := 0
	if len(plans) > 0 {
		now, err := st.assignChairs(ctx, plans)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		assigned = len(plans)
		for _, p := range plans {
			enqueueRideWrite(&rideWrite{rideID: p.RideID, chairID: p.ChairID, at: now})
		}
		st.finishAssign(plans)
	}

	tDB = time.Since(dbStart)
	// 計測: 待ちライド数・空き椅子数・割り当て数を5秒ごとに1行だけ出す
	matchingStats.Lock()
	matchingStats.calls++
	matchingStats.waiting += len(rides)
	matchingStats.free += len(chairs)
	matchingStats.assigned += assigned
	if usedHungarian {
		matchingStats.hungarian++
	}
	matchingStats.maxDur = max(matchingStats.maxDur, time.Since(started))
	matchingStats.maxLock = max(matchingStats.maxLock, tLock)
	matchingStats.maxCalc = max(matchingStats.maxCalc, tCalc)
	matchingStats.maxDB = max(matchingStats.maxDB, tDB)
	if time.Since(matchingStats.lastLog) >= 5*time.Second {
		if matchingStats.calls > 0 {
			slog.Info("matching",
				"calls", matchingStats.calls,
				"avg_waiting", float64(matchingStats.waiting)/float64(matchingStats.calls),
				"avg_free_chairs", float64(matchingStats.free)/float64(matchingStats.calls),
				"assigned", matchingStats.assigned,
				"hungarian", matchingStats.hungarian,
				"max_ms", matchingStats.maxDur.Milliseconds(),
				"max_lock_ms", matchingStats.maxLock.Milliseconds(),
				"max_calc_ms", matchingStats.maxCalc.Milliseconds(),
				"max_db_ms", matchingStats.maxDB.Milliseconds())
		}
		matchingStats.lastLog = time.Now()
		matchingStats.calls, matchingStats.waiting, matchingStats.free, matchingStats.assigned = 0, 0, 0, 0
		matchingStats.hungarian, matchingStats.maxDur = 0, 0
		matchingStats.maxLock, matchingStats.maxCalc, matchingStats.maxDB = 0, 0, 0
	}
	matchingStats.Unlock()

	w.WriteHeader(http.StatusNoContent)
}
