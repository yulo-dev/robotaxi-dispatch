// Package assign solves ride-to-vehicle assignment over a cost matrix.
//
// A cost matrix has one row per ride and one column per vehicle. An entry of
// +Inf means the pair is not allowed (vehicle is not a candidate for that
// ride). Every solver returns a slice with one entry per row: the assigned
// column, or -1 when the row is left unassigned. The package has no I/O and
// no third-party dependencies.
package assign

import "math"

// Inf marks a forbidden pair.
var Inf = math.Inf(1)

// Unassigned is the column value for a row with no assignment.
const Unassigned = -1

// Build returns a rides x vehicles matrix filled by cost(r, v).
func Build(rides, vehicles int, cost func(r, v int) float64) [][]float64 {
	m := make([][]float64, rides)
	for r := range m {
		m[r] = make([]float64, vehicles)
		for v := range m[r] {
			m[r][v] = cost(r, v)
		}
	}
	return m
}

// Total returns the summed cost and number of assigned rows.
func Total(cost [][]float64, a []int) (sum float64, matched int) {
	for r, c := range a {
		if c >= 0 {
			sum += cost[r][c]
			matched++
		}
	}
	return sum, matched
}

// Method names the solver used by Solve.
type Method string

const (
	MethodHungarian Method = "hungarian"
	MethodGreedy    Method = "greedy"
)

// Solve uses Hungarian when both dimensions are within maxDim, otherwise
// Greedy. maxDim <= 0 means no cap.
func Solve(cost [][]float64, maxDim int) ([]int, Method) {
	rows := len(cost)
	cols := 0
	if rows > 0 {
		cols = len(cost[0])
	}
	if maxDim > 0 && (rows > maxDim || cols > maxDim) {
		return Greedy(cost), MethodGreedy
	}
	return Hungarian(cost), MethodHungarian
}

// Greedy processes rows in order (request order); each row takes the
// cheapest still-free column with finite cost. Ties go to the lower column.
func Greedy(cost [][]float64) []int {
	out := make([]int, len(cost))
	if len(cost) == 0 {
		return out
	}
	taken := make([]bool, len(cost[0]))
	for r, row := range cost {
		best, bestC := Unassigned, Inf
		for c, x := range row {
			if !taken[c] && x < bestC {
				best, bestC = c, x
			}
		}
		out[r] = best
		if best >= 0 {
			taken[best] = true
		}
	}
	return out
}
