package main

import (
	"math"
	"math/rand"
	"testing"
)

// ハンガリアン法の結果が、総当たりで求めた最小コストと一致すること（行 <= 列）
func TestHungarianMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for iter := 0; iter < 300; iter++ {
		n := 1 + rng.Intn(5)
		m := n + rng.Intn(4)
		a := make([][]float64, n)
		for i := range a {
			a[i] = make([]float64, m)
			for j := range a[i] {
				a[i][j] = rng.Float64()*100 - 30 // 負のコスト（待ち時間補正）も含める
			}
		}
		assign := hungarian(a)
		got := 0.0
		usedCol := map[int]bool{}
		for i, j := range assign {
			if usedCol[j] {
				t.Fatalf("column %d assigned twice", j)
			}
			usedCol[j] = true
			got += a[i][j]
		}
		want := bruteMin(a, 0, make([]bool, m))
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("n=%d m=%d got %v want %v", n, m, got, want)
		}
	}
}

func bruteMin(a [][]float64, i int, used []bool) float64 {
	if i == len(a) {
		return 0
	}
	best := math.Inf(1)
	for j := range used {
		if used[j] {
			continue
		}
		used[j] = true
		best = math.Min(best, a[i][j]+bruteMin(a, i+1, used))
		used[j] = false
	}
	return best
}

// minCostAssign: 椅子が多い回・ライドが多い回の両方で、割り当てが重複せず、合計が総当たりの最小と一致すること。
// （依頼 < 空き椅子 の向きで添え字を取り違えて本番で panic した）
func TestMinCostAssignBothOrientations(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for iter := 0; iter < 300; iter++ {
		nChairs := 1 + rng.Intn(5)
		nRides := 1 + rng.Intn(5)
		cost := make([][]float64, nChairs)
		for i := range cost {
			cost[i] = make([]float64, 10)
			for j := range cost[i] {
				cost[i][j] = rng.Float64()*100 - 30
			}
		}
		// 候補ライドはライド番号の飛び飛びの部分集合（本番と同じく添え字 != ライド番号）
		cand := rng.Perm(10)[:nRides]
		got := minCostAssign(nChairs, cand, func(ci, ri int) float64 { return cost[ci][ri] })
		if len(got) != min(nChairs, nRides) {
			t.Fatalf("assigned %d, want %d", len(got), min(nChairs, nRides))
		}
		usedC, usedR, sum := map[int]bool{}, map[int]bool{}, 0.0
		for _, cr := range got {
			if usedC[cr[0]] || usedR[cr[1]] {
				t.Fatalf("duplicate assignment %v", got)
			}
			usedC[cr[0]], usedR[cr[1]] = true, true
			sum += cost[cr[0]][cr[1]]
		}
		// 総当たり: 少ない側を行にした行列で最小を求める
		var a [][]float64
		if nChairs <= nRides {
			a = make([][]float64, nChairs)
			for i := range a {
				for _, r := range cand {
					a[i] = append(a[i], cost[i][r])
				}
			}
		} else {
			a = make([][]float64, nRides)
			for i, r := range cand {
				for c := 0; c < nChairs; c++ {
					a[i] = append(a[i], cost[c][r])
				}
			}
		}
		want := bruteMin(a, 0, make([]bool, len(a[0])))
		if math.Abs(sum-want) > 1e-9 {
			t.Fatalf("chairs=%d rides=%d got %v want %v", nChairs, nRides, sum, want)
		}
	}
}
