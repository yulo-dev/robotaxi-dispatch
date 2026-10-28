package assign

import (
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// bruteForce returns the best achievable (matched, cost): maximum number of
// feasible pairs first, then minimum total cost.
func bruteForce(cost [][]float64) (int, float64) {
	rows := len(cost)
	if rows == 0 {
		return 0, 0
	}
	cols := len(cost[0])
	used := make([]bool, cols)
	bestN, bestC := -1, math.Inf(1)
	var rec func(r, n int, c float64)
	rec = func(r, n int, c float64) {
		if r == rows {
			if n > bestN || (n == bestN && c < bestC-1e-9) {
				bestN, bestC = n, c
			}
			return
		}
		rec(r+1, n, c) // leave row unassigned
		for j := 0; j < cols; j++ {
			if !used[j] && !math.IsInf(cost[r][j], 1) {
				used[j] = true
				rec(r+1, n+1, c+cost[r][j])
				used[j] = false
			}
		}
	}
	rec(0, 0, 0)
	return bestN, bestC
}

func randomMatrix(rng *rand.Rand, rows, cols int, infFrac float64) [][]float64 {
	return Build(rows, cols, func(int, int) float64 {
		if rng.Float64() < infFrac {
			return Inf
		}
		return math.Round(rng.Float64()*1000) / 10
	})
}

func checkValid(t *testing.T, cost [][]float64, a []int) {
	t.Helper()
	if len(a) != len(cost) {
		t.Fatalf("len %d, want %d", len(a), len(cost))
	}
	seen := map[int]bool{}
	for r, c := range a {
		if c == Unassigned {
			continue
		}
		if c < 0 || c >= len(cost[r]) {
			t.Fatalf("row %d: column %d out of range", r, c)
		}
		if seen[c] {
			t.Fatalf("column %d assigned twice", c)
		}
		seen[c] = true
		if math.IsInf(cost[r][c], 1) {
			t.Fatalf("row %d assigned forbidden column %d", r, c)
		}
	}
}

func TestHungarianMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 500; i++ {
		rows, cols := 1+rng.Intn(7), 1+rng.Intn(7)
		infFrac := []float64{0, 0.2, 0.5}[i%3]
		cost := randomMatrix(rng, rows, cols, infFrac)
		a := Hungarian(cost)
		checkValid(t, cost, a)
		got, gotN := Total(cost, a)
		wantN, want := bruteForce(cost)
		if gotN != wantN || math.Abs(got-want) > 1e-6 {
			t.Fatalf("case %d (%dx%d): hungarian matched=%d cost=%.3f, brute force matched=%d cost=%.3f\n%v",
				i, rows, cols, gotN, got, wantN, want, cost)
		}
	}
}

func TestHungarianRectangular(t *testing.T) {
	// 2 rides, 3 vehicles.
	wide := [][]float64{
		{4, 1, 3},
		{2, 0, 5},
	}
	a := Hungarian(wide)
	if s, n := Total(wide, a); n != 2 || s != 3 { // optimum is 3 (two optimal assignments)
		t.Fatalf("wide: got cost %v matched %d (%v)", s, n, a)
	}
	// 3 rides, 2 vehicles: one ride must stay unassigned.
	tall := [][]float64{
		{1, 9},
		{9, 1},
		{0, 0},
	}
	a = Hungarian(tall)
	checkValid(t, tall, a)
	if s, n := Total(tall, a); n != 2 || s != 1 {
		t.Fatalf("tall: got cost %v matched %d (%v)", s, n, a)
	}
}

func TestHungarianAllInfRow(t *testing.T) {
	cost := [][]float64{
		{Inf, Inf, Inf},
		{1, 2, 3},
		{Inf, 5, Inf},
	}
	a := Hungarian(cost)
	checkValid(t, cost, a)
	if a[0] != Unassigned {
		t.Fatalf("all-inf row assigned to %d", a[0])
	}
	if a[1] != 0 || a[2] != 1 {
		t.Fatalf("got %v", a)
	}
	allInf := [][]float64{{Inf, Inf}, {Inf, Inf}}
	for r, c := range Hungarian(allInf) {
		if c != Unassigned {
			t.Fatalf("row %d assigned in all-inf matrix", r)
		}
	}
}

func TestHungarianEmpty(t *testing.T) {
	if len(Hungarian(nil)) != 0 {
		t.Fatal("nil")
	}
	a := Hungarian([][]float64{{}, {}})
	if a[0] != Unassigned || a[1] != Unassigned {
		t.Fatal("zero columns")
	}
}

func TestGreedyRequestOrder(t *testing.T) {
	// Ride 0 is first and takes vehicle 0 even though ride 1 needs it more.
	cost := [][]float64{
		{1, 2},
		{1, 100},
	}
	a := Greedy(cost)
	if a[0] != 0 || a[1] != 1 {
		t.Fatalf("greedy got %v", a)
	}
	h := Hungarian(cost)
	if h[0] != 1 || h[1] != 0 {
		t.Fatalf("hungarian got %v", h)
	}
}

func TestGreedyValidAndNeverBetterThanHungarian(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for i := 0; i < 300; i++ {
		cost := randomMatrix(rng, 1+rng.Intn(20), 1+rng.Intn(20), 0.3)
		g := Greedy(cost)
		checkValid(t, cost, g)
		h := Hungarian(cost)
		gs, gn := Total(cost, g)
		hs, hn := Total(cost, h)
		if gn > hn || (gn == hn && gs < hs-1e-6) {
			t.Fatalf("case %d: greedy (%d, %.2f) beat hungarian (%d, %.2f)", i, gn, gs, hn, hs)
		}
	}
}

func TestSolveCap(t *testing.T) {
	cost := randomMatrix(rand.New(rand.NewSource(3)), 5, 5, 0)
	if _, m := Solve(cost, 4); m != MethodGreedy {
		t.Fatalf("over cap used %s", m)
	}
	if _, m := Solve(cost, 5); m != MethodHungarian {
		t.Fatalf("within cap used %s", m)
	}
	if _, m := Solve(cost, 0); m != MethodHungarian {
		t.Fatalf("no cap used %s", m)
	}
}

func BenchmarkHungarian(b *testing.B) {
	for _, n := range []int{50, 100, 200} {
		cost := randomMatrix(rand.New(rand.NewSource(int64(n))), n, n, 0)
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				Hungarian(cost)
			}
		})
	}
}

func BenchmarkGreedy(b *testing.B) {
	for _, n := range []int{50, 100, 200} {
		cost := randomMatrix(rand.New(rand.NewSource(int64(n))), n, n, 0)
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				Greedy(cost)
			}
		})
	}
}
