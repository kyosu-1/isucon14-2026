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
