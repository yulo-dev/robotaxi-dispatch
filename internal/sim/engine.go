package sim

import (
	"math"
	"sort"
	"time"

	"github.com/yulo-dev/robotaxi-dispatch/internal/assign"
	"github.com/yulo-dev/robotaxi-dispatch/internal/geo"
)

// Strategy selects the decision cost and the assignment solver.
type Strategy struct {
	Name      string
	RoadCost  bool // true: A* road ETA; false: straight-line ETA
	Hungarian bool // true: Hungarian; false: greedy in request order
}

// Strategies are the four compared strategies, in report order.
var Strategies = []Strategy{
	{Name: "greedy-straight", RoadCost: false, Hungarian: false},
	{Name: "greedy-road", RoadCost: true, Hungarian: false},
	{Name: "hungarian-straight", RoadCost: false, Hungarian: true},
	{Name: "hungarian-road", RoadCost: true, Hungarian: true},
}

// StrategyByName looks up a strategy.
func StrategyByName(name string) (Strategy, bool) {
	for _, s := range Strategies {
		if s.Name == name {
			return s, true
		}
	}
	return Strategy{}, false
}

// RideStatus is the terminal or current state of a simulated ride.
type RideStatus int

const (
	RideOpen RideStatus = iota
	RideAssigned
	RideCompleted
	RideFailed
)

// RideRecord is the outcome of one ride. Times are seconds since start.
type RideRecord struct {
	ID          int
	Status      RideStatus
	VehicleID   int // -1 if never assigned
	RequestT    float64
	AssignT     float64
	PickupT     float64
	DropT       float64
	Attempts    int
	PickupM     float64
	TripM       float64
	CompletedAt float64
}

// PickedUp reports whether the vehicle reached the rider by time end.
func (r RideRecord) PickedUp(end float64) bool {
	return (r.Status == RideAssigned || r.Status == RideCompleted) && r.PickupT <= end
}

type vehicle struct {
	node     geo.NodeID
	busy     bool
	freeTick int
	ride     int
}

type openRide struct {
	idx      int
	excluded []int
}

// Run simulates one strategy on a scenario. The road network is the ground
// truth for movement: every vehicle travels the A* route, whatever cost the
// strategy used to decide.
func Run(n *geo.Network, sc Scenario, cfg Config, st Strategy) Result {
	started := time.Now()
	router := geo.NewRouter(n, cfg.CacheSize)
	straightMps := geo.KmhToMps(cfg.StraightKmh)

	rides := make([]RideRecord, len(sc.Requests))
	for i, r := range sc.Requests {
		rides[i] = RideRecord{ID: r.ID, VehicleID: -1, RequestT: float64(r.T)}
	}
	fleet := make([]vehicle, len(sc.VehicleStart))
	for i, node := range sc.VehicleStart {
		fleet[i] = vehicle{node: node, ride: -1}
	}

	var open []openRide
	next := 0 // next request to arrive
	busy := 0
	maxTick := cfg.DurationSec + 24*3600 // hard stop for drain mode

	t := 0
	for ; ; t++ {
		// 1. Vehicles that finished a trip become available at their drop node.
		for vi := range fleet {
			v := &fleet[vi]
			if v.busy && v.freeTick <= t {
				rides[v.ride].Status = RideCompleted
				rides[v.ride].CompletedAt = rides[v.ride].DropT
				v.busy, v.ride = false, -1
				busy--
			}
		}
		// 2. New requests enter the queue.
		for next < len(sc.Requests) && sc.Requests[next].T <= t {
			open = append(open, openRide{idx: next})
			next++
		}
		// 3. Dispatcher runs at the end of each batch window.
		if t%cfg.BatchSec == 0 && len(open) > 0 {
			open = expire(open, rides, t, cfg.MatchTimeout)
			busy += dispatch(t, open, rides, fleet, sc, cfg, st, router, straightMps)
			open = compactOpen(open, rides)
		}

		if cfg.Drain {
			if (t >= cfg.DurationSec && next == len(sc.Requests) && len(open) == 0 && busy == 0) || t >= maxTick {
				break
			}
		} else if t >= cfg.DurationSec {
			break
		}
	}

	res := summarize(rides, router.Stats(), float64(t))
	res.Strategy = st.Name
	res.Seed = sc.Seed
	res.RuntimeMs = float64(time.Since(started).Microseconds()) / 1000
	res.Rides = rides
	return res
}

func expire(open []openRide, rides []RideRecord, t, timeout int) []openRide {
	out := open[:0]
	for _, o := range open {
		if float64(t)-rides[o.idx].RequestT >= float64(timeout) {
			rides[o.idx].Status = RideFailed
			continue
		}
		out = append(out, o)
	}
	return out
}

func compactOpen(open []openRide, rides []RideRecord) []openRide {
	out := open[:0]
	for _, o := range open {
		if rides[o.idx].Status == RideOpen {
			out = append(out, o)
		}
	}
	return out
}

// dispatch runs one batch and returns the number of vehicles that became busy.
func dispatch(t int, open []openRide, rides []RideRecord, fleet []vehicle, sc Scenario,
	cfg Config, st Strategy, router *geo.Router, straightMps float64) int {
	n := router.Network()
	var idle []int
	for vi := range fleet {
		if !fleet[vi].busy {
			idle = append(idle, vi)
		}
	}
	if len(idle) == 0 {
		return 0
	}

	// Candidate search: straight-line radius (like a GEO index), top K nearest.
	type cand struct {
		v int
		d float64
	}
	colOf := map[int]int{}
	var cols []int
	candidates := make([][]cand, len(open))
	for ri, o := range open {
		req := sc.Requests[o.idx]
		var within []cand
		for _, radius := range []float64{cfg.RadiusM, cfg.ExpandM} {
			within = within[:0]
			for _, vi := range idle {
				if contains(o.excluded, vi) {
					continue
				}
				d := geo.Haversine(n.Node(fleet[vi].node), req.Pickup)
				if d <= radius {
					within = append(within, cand{vi, d})
				}
			}
			if len(within) > 0 {
				break
			}
		}
		sort.Slice(within, func(a, b int) bool {
			if within[a].d != within[b].d {
				return within[a].d < within[b].d
			}
			return within[a].v < within[b].v
		})
		if len(within) > cfg.TopK {
			within = within[:cfg.TopK]
		}
		candidates[ri] = within
		for _, c := range within {
			if _, ok := colOf[c.v]; !ok {
				colOf[c.v] = len(cols)
				cols = append(cols, c.v)
			}
		}
	}
	if len(cols) == 0 {
		return 0
	}

	cost := make([][]float64, len(open))
	for ri, o := range open {
		row := make([]float64, len(cols))
		for j := range row {
			row[j] = assign.Inf
		}
		req := sc.Requests[o.idx]
		for _, c := range candidates[ri] {
			from := fleet[c.v].node
			var eta float64
			if st.RoadCost {
				p, err := router.Route(from, req.PickupNode)
				if err != nil {
					continue
				}
				eta = p.Seconds
			} else {
				eta = geo.StraightLineETA(n.Node(from), n.Node(req.PickupNode), straightMps)
			}
			row[colOf[c.v]] = eta
		}
		cost[ri] = row
	}

	var a []int
	if st.Hungarian {
		a, _ = assign.Solve(cost, cfg.MaxHungarian)
	} else {
		a = assign.Greedy(cost)
	}

	started := 0
	for ri, col := range a {
		if col < 0 {
			continue
		}
		vi := cols[col]
		o := &open[ri]
		rec := &rides[o.idx]
		rec.Attempts++
		if !accepts(sc.Seed, rec.ID, rec.Attempts, cfg.AcceptProb) {
			o.excluded = append(o.excluded, vi)
			continue
		}
		req := sc.Requests[o.idx]
		v := &fleet[vi]
		toPickup, err1 := router.Route(v.node, req.PickupNode)
		trip, err2 := router.Route(req.PickupNode, req.DropNode)
		if err1 != nil || err2 != nil {
			o.excluded = append(o.excluded, vi)
			continue
		}
		rec.Status = RideAssigned
		rec.VehicleID = vi
		rec.AssignT = float64(t)
		rec.PickupT = rec.AssignT + toPickup.Seconds
		rec.DropT = rec.PickupT + trip.Seconds
		rec.PickupM = toPickup.Meters
		rec.TripM = trip.Meters
		v.busy = true
		v.ride = o.idx
		v.node = req.DropNode
		v.freeTick = int(math.Ceil(rec.DropT))
		if v.freeTick <= t {
			v.freeTick = t + 1
		}
		started++
	}
	return started
}

func contains(xs []int, x int) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

// accepts draws the accept/reject outcome for (ride, attempt) from a hash of
// the seed, so every strategy sees the same draw for the same attempt.
func accepts(seed int64, rideID, attempt int, p float64) bool {
	x := uint64(seed)*0x9E3779B97F4A7C15 ^ uint64(rideID)*0xBF58476D1CE4E5B9 ^ uint64(attempt)*0x94D049BB133111EB
	x ^= x >> 30
	x *= 0xBF58476D1CE4E5B9
	x ^= x >> 27
	x *= 0x94D049BB133111EB
	x ^= x >> 31
	return float64(x>>11)/(1<<53) < p
}
