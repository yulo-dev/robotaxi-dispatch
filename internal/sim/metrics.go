package sim

import (
	"math"
	"sort"

	"github.com/yulo-dev/robotaxi-dispatch/internal/geo"
)

// Result holds the metrics of one (seed, strategy) run.
type Result struct {
	Strategy string `json:"strategy"`
	Seed     int64  `json:"seed"`

	Requested int `json:"requested"`
	Assigned  int `json:"assigned"`
	Completed int `json:"completed"`
	Failed    int `json:"failed"`
	Open      int `json:"open"` // unresolved when the run stopped (0 in drain mode)

	MatchRate      float64 `json:"match_rate"`          // assigned / requested
	PickupWaitP50  float64 `json:"pickup_wait_p50_s"`   // PickupT - RequestT, picked-up rides
	PickupWaitP95  float64 `json:"pickup_wait_p95_s"`   //
	PickupWaitAvg  float64 `json:"pickup_wait_avg_s"`   //
	TimeToMatchP95 float64 `json:"time_to_match_p95_s"` // AssignT - RequestT, assigned rides
	EmptyKmPct     float64 `json:"empty_km_pct"`        // pickup km / total km driven
	AvgPickupKm    float64 `json:"avg_pickup_km"`       // per picked-up ride
	CacheHitRate   float64 `json:"astar_cache_hit_rate"`
	EndT           float64 `json:"end_s"`
	RuntimeMs      float64 `json:"runtime_ms"`

	Rides []RideRecord `json:"-"`
}

func summarize(rides []RideRecord, stats geo.RouteStats, end float64) Result {
	r := Result{Requested: len(rides), EndT: end, CacheHitRate: stats.HitRate()}
	var waits, matches []float64
	var emptyM, totalM float64
	for _, x := range rides {
		switch x.Status {
		case RideOpen:
			r.Open++
		case RideFailed:
			r.Failed++
		case RideAssigned:
			r.Open++
			r.Assigned++
		case RideCompleted:
			r.Completed++
			r.Assigned++
		}
		if x.Status == RideAssigned || x.Status == RideCompleted {
			matches = append(matches, x.AssignT-x.RequestT)
			// Distance is counted for every dispatched vehicle, including
			// trips still in progress at the end of a non-drain run.
			emptyM += x.PickupM
			totalM += x.PickupM + x.TripM
		}
		if x.PickedUp(end) {
			waits = append(waits, x.PickupT-x.RequestT)
		}
	}
	if r.Requested > 0 {
		r.MatchRate = float64(r.Assigned) / float64(r.Requested)
	}
	r.PickupWaitP50 = percentile(waits, 50)
	r.PickupWaitP95 = percentile(waits, 95)
	r.PickupWaitAvg = mean(waits)
	r.TimeToMatchP95 = percentile(matches, 95)
	if totalM > 0 {
		r.EmptyKmPct = 100 * emptyM / totalM
	}
	if len(matches) > 0 {
		r.AvgPickupKm = emptyM / 1000 / float64(len(matches))
	}
	return r
}

// percentile uses linear interpolation between closest ranks. Returns 0 for
// an empty input.
func percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]float64(nil), xs...)
	sort.Float64s(s)
	pos := p / 100 * float64(len(s)-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	return s[lo] + (s[hi]-s[lo])*(pos-float64(lo))
}

func mean(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	s := 0.0
	for _, x := range xs {
		s += x
	}
	return s / float64(len(xs))
}

// Stat is a mean and sample standard deviation over seeds.
type Stat struct {
	Mean float64 `json:"mean"`
	Std  float64 `json:"std"`
}

func statOf(xs []float64) Stat {
	m := mean(xs)
	if len(xs) < 2 {
		return Stat{Mean: m}
	}
	ss := 0.0
	for _, x := range xs {
		ss += (x - m) * (x - m)
	}
	return Stat{Mean: m, Std: math.Sqrt(ss / float64(len(xs)-1))}
}

// Aggregate summarizes one strategy across seeds.
type Aggregate struct {
	Strategy       string `json:"strategy"`
	Seeds          int    `json:"seeds"`
	Requested      Stat   `json:"requested"`
	Completed      Stat   `json:"completed"`
	Failed         Stat   `json:"failed"`
	MatchRate      Stat   `json:"match_rate"`
	PickupWaitP50  Stat   `json:"pickup_wait_p50_s"`
	PickupWaitP95  Stat   `json:"pickup_wait_p95_s"`
	PickupWaitAvg  Stat   `json:"pickup_wait_avg_s"`
	TimeToMatchP95 Stat   `json:"time_to_match_p95_s"`
	EmptyKmPct     Stat   `json:"empty_km_pct"`
	AvgPickupKm    Stat   `json:"avg_pickup_km"`
	CacheHitRate   Stat   `json:"astar_cache_hit_rate"`
	RuntimeMs      Stat   `json:"runtime_ms"`
}

// AggregateResults groups results by strategy and computes mean and std.
func AggregateResults(strategy string, rs []Result) Aggregate {
	pick := func(f func(Result) float64) Stat {
		xs := make([]float64, len(rs))
		for i, r := range rs {
			xs[i] = f(r)
		}
		return statOf(xs)
	}
	return Aggregate{
		Strategy:       strategy,
		Seeds:          len(rs),
		Requested:      pick(func(r Result) float64 { return float64(r.Requested) }),
		Completed:      pick(func(r Result) float64 { return float64(r.Completed) }),
		Failed:         pick(func(r Result) float64 { return float64(r.Failed) }),
		MatchRate:      pick(func(r Result) float64 { return r.MatchRate }),
		PickupWaitP50:  pick(func(r Result) float64 { return r.PickupWaitP50 }),
		PickupWaitP95:  pick(func(r Result) float64 { return r.PickupWaitP95 }),
		PickupWaitAvg:  pick(func(r Result) float64 { return r.PickupWaitAvg }),
		TimeToMatchP95: pick(func(r Result) float64 { return r.TimeToMatchP95 }),
		EmptyKmPct:     pick(func(r Result) float64 { return r.EmptyKmPct }),
		AvgPickupKm:    pick(func(r Result) float64 { return r.AvgPickupKm }),
		CacheHitRate:   pick(func(r Result) float64 { return r.CacheHitRate }),
		RuntimeMs:      pick(func(r Result) float64 { return r.RuntimeMs }),
	}
}
