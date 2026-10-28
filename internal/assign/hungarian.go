package assign

import "math"

// Hungarian returns a minimum-cost assignment in O(n^2 * m) time, where
// n = min(rows, cols) and m = max(rows, cols) (O(n^3) for square input).
//
// Rectangular input is handled by solving with the shorter side as rows
// (equivalent to padding with zero-cost dummy rows). Forbidden (+Inf) pairs
// are replaced by a penalty larger than the total cost of any assignment, so
// the solver first maximizes the number of feasible pairs and then minimizes
// their total cost. Pairs that still land on a forbidden entry are reported
// as Unassigned.
func Hungarian(cost [][]float64) []int {
	rows := len(cost)
	out := make([]int, rows)
	for i := range out {
		out[i] = Unassigned
	}
	if rows == 0 || len(cost[0]) == 0 {
		return out
	}
	cols := len(cost[0])

	// Penalty for forbidden pairs: exceeds any sum of finite entries.
	maxAbs := 0.0
	for _, row := range cost {
		for _, x := range row {
			if !math.IsInf(x, 0) && !math.IsNaN(x) {
				maxAbs = math.Max(maxAbs, math.Abs(x))
			}
		}
	}
	k := rows
	if cols < k {
		k = cols
	}
	big := (maxAbs+1)*float64(k+1) + 1

	transpose := rows > cols
	n, m := rows, cols
	if transpose {
		n, m = cols, rows
	}
	at := func(i, j int) float64 { // i in [0,n), j in [0,m)
		var x float64
		if transpose {
			x = cost[j][i]
		} else {
			x = cost[i][j]
		}
		if math.IsInf(x, 0) || math.IsNaN(x) {
			return big
		}
		return x
	}

	// Shortest augmenting path with potentials (1-indexed; column 0 is the
	// virtual source). u, v are dual potentials; p[j] is the row matched to
	// column j; way[j] is the previous column on the alternating path.
	u := make([]float64, n+1)
	v := make([]float64, m+1)
	p := make([]int, m+1)
	way := make([]int, m+1)
	minv := make([]float64, m+1)
	used := make([]bool, m+1)
	for i := 1; i <= n; i++ {
		p[0] = i
		j0 := 0
		for j := range minv {
			minv[j] = math.Inf(1)
			used[j] = false
		}
		for {
			used[j0] = true
			i0 := p[j0]
			delta := math.Inf(1)
			j1 := 0
			for j := 1; j <= m; j++ {
				if used[j] {
					continue
				}
				cur := at(i0-1, j-1) - u[i0] - v[j]
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

	for j := 1; j <= m; j++ {
		i := p[j]
		if i == 0 {
			continue
		}
		r, c := i-1, j-1
		if transpose {
			r, c = c, r
		}
		x := cost[r][c]
		if math.IsInf(x, 0) || math.IsNaN(x) {
			continue
		}
		out[r] = c
	}
	return out
}
