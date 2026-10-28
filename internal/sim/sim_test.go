package sim

import (
	"math"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/yulo-dev/robotaxi-dispatch/internal/geo"
)

func smallConfig() Config {
	cfg := DefaultConfig()
	cfg.Vehicles = 25
	cfg.DurationSec = 20 * 60
	cfg.RatePerHour = 200
	return cfg
}

var testNetCache *geo.Network

func network(t *testing.T) *geo.Network {
	t.Helper()
	if testNetCache == nil {
		n, err := geo.NewGrid(DefaultConfig().Grid)
		if err != nil {
			t.Fatal(err)
		}
		testNetCache = n
	}
	return testNetCache
}

func stripRuntime(r Result) Result {
	r.RuntimeMs = 0
	return r
}

func TestDeterminism(t *testing.T) {
	n := network(t)
	cfg := smallConfig()
	for _, st := range Strategies {
		a := Run(n, NewScenario(n, cfg, 7), cfg, st)
		b := Run(n, NewScenario(n, cfg, 7), cfg, st)
		if !reflect.DeepEqual(stripRuntime(a), stripRuntime(b)) {
			t.Fatalf("%s: results differ between identical runs", st.Name)
		}
		if a.Requested == 0 || a.Completed == 0 {
			t.Fatalf("%s: empty run %+v", st.Name, a)
		}
	}
}

func TestParallelismDoesNotChangeResults(t *testing.T) {
	n := network(t)
	cfg := smallConfig()
	a := RunExperiment(n, cfg, SeedList(2), Strategies, 1)
	b := RunExperiment(n, cfg, SeedList(2), Strategies, 4)
	for i := range a.Results {
		if !reflect.DeepEqual(stripRuntime(a.Results[i]), stripRuntime(b.Results[i])) {
			t.Fatalf("result %d differs with parallel runs", i)
		}
	}
}

func TestDifferentSeedsDiffer(t *testing.T) {
	n := network(t)
	cfg := smallConfig()
	a := NewScenario(n, cfg, 1)
	b := NewScenario(n, cfg, 2)
	if reflect.DeepEqual(a.Requests, b.Requests) {
		t.Fatal("different seeds produced identical demand")
	}
}

func TestPickupWaitAtLeastTimeToMatch(t *testing.T) {
	n := network(t)
	cfg := smallConfig()
	for _, st := range Strategies {
		res := Run(n, NewScenario(n, cfg, 3), cfg, st)
		for _, r := range res.Rides {
			if !r.PickedUp(res.EndT) {
				continue
			}
			wait, match := r.PickupT-r.RequestT, r.AssignT-r.RequestT
			if match < 0 || wait+1e-9 < match {
				t.Fatalf("%s ride %d: wait %.1f < time to match %.1f", st.Name, r.ID, wait, match)
			}
		}
	}
}

func TestVehicleNeverHasTwoRides(t *testing.T) {
	n := network(t)
	cfg := smallConfig()
	cfg.Vehicles = 10 // force heavy reuse
	for _, st := range Strategies {
		res := Run(n, NewScenario(n, cfg, 4), cfg, st)
		byVehicle := map[int][][2]float64{}
		for _, r := range res.Rides {
			if r.VehicleID >= 0 {
				byVehicle[r.VehicleID] = append(byVehicle[r.VehicleID], [2]float64{r.AssignT, r.DropT})
			}
		}
		reused := false
		for v, iv := range byVehicle {
			sort.Slice(iv, func(a, b int) bool { return iv[a][0] < iv[b][0] })
			for i := 1; i < len(iv); i++ {
				reused = true
				if iv[i][0] < iv[i-1][1] {
					t.Fatalf("%s: vehicle %d assigned at %.0f while busy until %.0f", st.Name, v, iv[i][0], iv[i-1][1])
				}
			}
		}
		if !reused {
			t.Fatalf("%s: test did not exercise vehicle reuse", st.Name)
		}
	}
}

func TestConservation(t *testing.T) {
	n := network(t)
	for _, drain := range []bool{false, true} {
		cfg := smallConfig()
		cfg.Drain = drain
		for _, st := range Strategies {
			res := Run(n, NewScenario(n, cfg, 5), cfg, st)
			if res.Completed+res.Failed+res.Open != res.Requested {
				t.Fatalf("drain=%v %s: completed %d + failed %d + open %d != requested %d",
					drain, st.Name, res.Completed, res.Failed, res.Open, res.Requested)
			}
			if drain && res.Open != 0 {
				t.Fatalf("drain run left %d rides open", res.Open)
			}
			if !drain && res.Open == 0 {
				t.Fatalf("%s: expected in-flight rides when stopping without drain", st.Name)
			}
		}
	}
}

func TestSameAcceptDrawsAcrossStrategies(t *testing.T) {
	draws := func() []bool {
		var out []bool
		for ride := 0; ride < 100; ride++ {
			for attempt := 1; attempt < 4; attempt++ {
				out = append(out, accepts(1, ride, attempt, 0.9))
			}
		}
		return out
	}
	if !reflect.DeepEqual(draws(), draws()) {
		t.Fatal("accept draws not deterministic")
	}
	// Roughly the configured probability.
	ok := 0
	for ride := 0; ride < 10000; ride++ {
		if accepts(3, ride, 1, 0.9) {
			ok++
		}
	}
	if ok < 8800 || ok > 9200 {
		t.Fatalf("accept rate %d/10000", ok)
	}
}

func TestDemandShape(t *testing.T) {
	n := network(t)
	cfg := DefaultConfig()
	sc := NewScenario(n, cfg, 1)
	// Expected count: rate * (2/3 * 1 + 1/3 * peak) * hours.
	hours := float64(cfg.DurationSec) / 3600
	want := cfg.RatePerHour * hours * (2.0/3 + cfg.PeakMult/3)
	got := float64(len(sc.Requests))
	if math.Abs(got-want) > 4*math.Sqrt(want) {
		t.Fatalf("requests %v, expected about %.0f", got, want)
	}
	if len(sc.Hotspots) < 3 || len(sc.Hotspots) > 5 {
		t.Fatalf("hotspots %d", len(sc.Hotspots))
	}
	for i, r := range sc.Requests {
		d := geo.Haversine(r.Pickup, r.Dropoff)
		if d < minTripM-1 || d > maxTripM+50 {
			t.Fatalf("request %d trip %.0f m outside 1-5 km", i, d)
		}
		if i > 0 && r.T < sc.Requests[i-1].T {
			t.Fatal("requests not sorted by time")
		}
	}
}

func TestPercentile(t *testing.T) {
	xs := []float64{5, 1, 3, 2, 4}
	if percentile(xs, 50) != 3 || percentile(xs, 0) != 1 || percentile(xs, 100) != 5 {
		t.Fatal("percentile")
	}
	if percentile(nil, 95) != 0 {
		t.Fatal("empty percentile")
	}
	if got := percentile([]float64{0, 10}, 95); math.Abs(got-9.5) > 1e-9 {
		t.Fatalf("interp %v", got)
	}
}

func TestGoBenchTable(t *testing.T) {
	in := "goos: linux\nBenchmarkHungarian/n=50-2   \t 9382\t 112137 ns/op\t 2560 B/op\nPASS\n"
	var sb strings.Builder
	if err := WriteGoBenchTable(&sb, strings.NewReader(in)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sb.String(), "| BenchmarkHungarian/n=50 | 9382 | 112137 | 0.112 |") {
		t.Fatalf("got %q", sb.String())
	}
}
