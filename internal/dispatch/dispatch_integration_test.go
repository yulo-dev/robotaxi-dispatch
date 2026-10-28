//go:build integration

package dispatch_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"sync"
	"testing"
	"time"

	"github.com/yulo-dev/robotaxi-dispatch/internal/cache"
	"github.com/yulo-dev/robotaxi-dispatch/internal/dispatch"
	"github.com/yulo-dev/robotaxi-dispatch/internal/geo"
	"github.com/yulo-dev/robotaxi-dispatch/internal/store"
	"github.com/yulo-dev/robotaxi-dispatch/internal/testdb"
)

var (
	netOnce sync.Once
	router  *geo.Router
)

type env struct {
	st  *store.Store
	c   *cache.Cache
	svc *dispatch.Service
	cfg dispatch.Config
}

func setup(t *testing.T, mutate func(*dispatch.Config)) *env {
	t.Helper()
	netOnce.Do(func() {
		n, err := geo.NewGrid(geo.DefaultGridConfig())
		if err != nil {
			panic(err)
		}
		router = geo.NewRouter(n, 10_000)
	})
	st := testdb.Open(t)
	c := testdb.Redis(t)
	cfg := dispatch.DefaultConfig()
	cfg.RetryDelay = time.Hour // tests drive retries explicitly
	if mutate != nil {
		mutate(&cfg)
	}
	return &env{st: st, c: c, svc: dispatch.New(cfg, st, c, router, nil, nil), cfg: cfg}
}

func (e *env) vehicle(t *testing.T, id string, lat, lng float64) {
	t.Helper()
	v, err := e.st.CreateVehicle(context.Background(), store.Vehicle{ID: id, Status: store.VehicleAvailable, Lat: lat, Lng: lng, BatteryPct: 90})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.c.UpsertVehicle(context.Background(), v); err != nil {
		t.Fatal(err)
	}
}

func (e *env) ride(t *testing.T, key string, lat, lng float64) string {
	t.Helper()
	resp, _, err := e.st.CreateRide(context.Background(), key, key, store.NewRide{
		RiderID: "r-" + key, PickupLat: lat, PickupLng: lng, DropoffLat: lat + 0.01, DropoffLng: lng,
	}, func(r store.Ride) store.StoredResponse {
		b, _ := json.Marshal(r)
		return store.StoredResponse{Status: 202, Body: b}
	})
	if err != nil {
		t.Fatal(err)
	}
	var r store.Ride
	_ = json.Unmarshal(resp.Body, &r)
	return r.ID
}

func (e *env) getRide(t *testing.T, id string) store.Ride {
	t.Helper()
	r, err := e.st.GetRide(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (e *env) getVehicle(t *testing.T, id string) store.Vehicle {
	t.Helper()
	v, err := e.st.GetVehicle(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (e *env) inGeo(t *testing.T, id string) bool {
	t.Helper()
	pos, err := e.c.Client().GeoPos(context.Background(), "vehicles:geo", id).Result()
	if err != nil {
		t.Fatal(err)
	}
	return len(pos) == 1 && pos[0] != nil
}

// checkInvariants asserts no vehicle has two active rides and that every
// busy vehicle is referenced by exactly one active ride.
func (e *env) checkInvariants(t *testing.T) (active int) {
	t.Helper()
	ctx := context.Background()
	var dup int
	if err := e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM (SELECT vehicle_id FROM rides
		WHERE status IN ('DISPATCHED','ASSIGNED','IN_TRIP') GROUP BY vehicle_id HAVING count(*) > 1) d`).Scan(&dup); err != nil {
		t.Fatal(err)
	}
	if dup != 0 {
		t.Fatalf("%d vehicles have more than one active ride", dup)
	}
	var mismatch int
	if err := e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM vehicles v
		WHERE (v.status IN ('DISPATCHED','EN_ROUTE','IN_TRIP')) <>
		      EXISTS (SELECT 1 FROM rides r WHERE r.vehicle_id = v.id AND r.status IN ('DISPATCHED','ASSIGNED','IN_TRIP'))`).Scan(&mismatch); err != nil {
		t.Fatal(err)
	}
	if mismatch != 0 {
		t.Fatalf("%d vehicles whose status disagrees with their rides", mismatch)
	}
	var noVehicle int
	if err := e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM rides WHERE status IN ('DISPATCHED','ASSIGNED','IN_TRIP') AND vehicle_id IS NULL`).Scan(&noVehicle); err != nil {
		t.Fatal(err)
	}
	if noVehicle != 0 {
		t.Fatalf("%d active rides without a vehicle", noVehicle)
	}
	if err := e.st.Pool().QueryRow(ctx, `SELECT count(*) FROM rides WHERE status IN ('DISPATCHED','ASSIGNED','IN_TRIP')`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	return active
}

func TestDispatchAssignsNearestVehicle(t *testing.T) {
	e := setup(t, nil)
	e.vehicle(t, "near", 37.7700, -122.4200)
	e.vehicle(t, "far", 37.7900, -122.4000)
	id := e.ride(t, "k1", 37.7705, -122.4205)
	if err := e.svc.ProcessBatch(context.Background(), []string{id}); err != nil {
		t.Fatal(err)
	}
	r := e.getRide(t, id)
	if r.Status != store.RideDispatched || r.VehicleID == nil || *r.VehicleID != "near" || r.DispatchedAt == nil {
		t.Fatalf("ride %+v", r)
	}
	if v := e.getVehicle(t, "near"); v.Status != store.VehicleDispatched {
		t.Fatalf("vehicle status %s", v.Status)
	}
	if e.inGeo(t, "near") || !e.inGeo(t, "far") {
		t.Fatal("geo index not updated")
	}
	// Redelivery of the same ride is a no-op.
	if err := e.svc.ProcessBatch(context.Background(), []string{id, id}); err != nil {
		t.Fatal(err)
	}
	if r2 := e.getRide(t, id); r2.Version != r.Version {
		t.Fatal("redelivered ride was modified")
	}
}

func TestBatchUsesOptimalAssignment(t *testing.T) {
	// Ride a (requested first) is slightly closer to X than to Y; ride b can
	// only reach X cheaply. Request-order greedy gives a->X, b->Y; the
	// minimum total ETA is a->Y, b->X.
	e := setup(t, nil)
	X, Y := geo.Location{Lat: 37.7700, Lng: -122.4300}, geo.Location{Lat: 37.7700, Lng: -122.4060}
	A, B := geo.Location{Lat: 37.7700, Lng: -122.4200}, geo.Location{Lat: 37.7700, Lng: -122.4400}
	eta := func(from, to geo.Location) float64 {
		s, err := router.ETA(from, to)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	if !(eta(X, A) < eta(Y, A) && eta(Y, A)+eta(X, B) < eta(X, A)+eta(Y, B)) {
		t.Fatal("fixture no longer separates greedy from optimal")
	}
	e.vehicle(t, "X", X.Lat, X.Lng)
	e.vehicle(t, "Y", Y.Lat, Y.Lng)
	a := e.ride(t, "a", A.Lat, A.Lng)
	b := e.ride(t, "b", B.Lat, B.Lng)
	if err := e.svc.ProcessBatch(context.Background(), []string{a, b}); err != nil {
		t.Fatal(err)
	}
	ra, rb := e.getRide(t, a), e.getRide(t, b)
	if ra.VehicleID == nil || rb.VehicleID == nil {
		t.Fatalf("not both dispatched: %v %v", ra.VehicleID, rb.VehicleID)
	}
	if *ra.VehicleID != "Y" || *rb.VehicleID != "X" {
		t.Fatalf("got a->%s b->%s, want a->Y b->X", *ra.VehicleID, *rb.VehicleID)
	}
}

func TestConcurrentWorkersNeverDoubleBookVehicles(t *testing.T) {
	e := setup(t, nil)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 5; i++ {
		e.vehicle(t, fmt.Sprintf("v%d", i), 37.77+rng.Float64()*0.01, -122.42+rng.Float64()*0.01)
	}
	var rides []string
	for i := 0; i < 20; i++ {
		rides = append(rides, e.ride(t, fmt.Sprintf("k%d", i), 37.77+rng.Float64()*0.01, -122.42+rng.Float64()*0.01))
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 20)
	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ids := append([]string(nil), rides...)
			r := rand.New(rand.NewSource(int64(w)))
			r.Shuffle(len(ids), func(i, j int) { ids[i], ids[j] = ids[j], ids[i] })
			<-start
			for round := 0; round < 3; round++ {
				if err := e.svc.ProcessBatch(context.Background(), ids); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if active := e.checkInvariants(t); active != 5 {
		t.Fatalf("%d active rides, want 5 (one per vehicle)", active)
	}
}

func TestDatabaseGuardsWithoutRedisLocks(t *testing.T) {
	// Bypass the Redis locks entirely: 20 goroutines call DispatchRide on
	// random pairs. Only the version check, the AVAILABLE condition, and the
	// partial unique index protect the data.
	e := setup(t, nil)
	ctx := context.Background()
	var vehicles, rides []string
	for i := 0; i < 5; i++ {
		id := fmt.Sprintf("v%d", i)
		e.vehicle(t, id, 37.77, -122.42)
		vehicles = append(vehicles, id)
	}
	for i := 0; i < 20; i++ {
		id := e.ride(t, fmt.Sprintf("k%d", i), 37.77, -122.42)
		r := e.getRide(t, id)
		if _, err := e.st.StartMatching(ctx, r); err != nil {
			t.Fatal(err)
		}
		rides = append(rides, id)
	}
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	conflicts := 0
	for w := 0; w < 20; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			<-start
			for i := 0; i < 40; i++ {
				rid := rides[rng.Intn(len(rides))]
				r, err := e.st.GetRide(ctx, rid)
				if err != nil {
					t.Error(err)
					return
				}
				_, _, err = e.st.DispatchRide(ctx, rid, r.Version, vehicles[rng.Intn(len(vehicles))])
				if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrVehicleBusy) {
					mu.Lock()
					conflicts++
					mu.Unlock()
					continue
				}
				if err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	close(start)
	wg.Wait()
	if active := e.checkInvariants(t); active != 5 {
		t.Fatalf("%d active rides, want 5", active)
	}
	if conflicts == 0 {
		t.Fatal("expected conflicts to be detected")
	}
}

func TestRejectReturnsRideAndVehicle(t *testing.T) {
	e := setup(t, nil)
	ctx := context.Background()
	e.vehicle(t, "v1", 37.77, -122.42)
	id := e.ride(t, "k1", 37.7701, -122.4201)
	if err := e.svc.ProcessBatch(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.Decide(ctx, id, "other", false); !errors.Is(err, store.ErrWrongVehicle) {
		t.Fatalf("wrong vehicle: %v", err)
	}
	r, err := e.svc.Decide(ctx, id, "v1", false)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != store.RideMatching || r.VehicleID != nil {
		t.Fatalf("after reject: %+v", r)
	}
	if v := e.getVehicle(t, "v1"); v.Status != store.VehicleAvailable || !e.inGeo(t, "v1") {
		t.Fatalf("vehicle not available again: %s", v.Status)
	}
	// The rejecting vehicle is excluded on the next attempt.
	if err := e.svc.ProcessBatch(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	if r := e.getRide(t, id); r.Status != store.RideMatching {
		t.Fatalf("rejected vehicle was offered the ride again: %s", r.Status)
	}
	// A second vehicle can take it.
	e.vehicle(t, "v2", 37.771, -122.421)
	if err := e.svc.ProcessBatch(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	if r := e.getRide(t, id); r.Status != store.RideDispatched || *r.VehicleID != "v2" {
		t.Fatalf("ride %+v", r)
	}
	e.checkInvariants(t)
}

func TestDecisionTimeoutReturnsRideAndVehicle(t *testing.T) {
	e := setup(t, func(c *dispatch.Config) { c.DecisionTimeout = 50 * time.Millisecond })
	ctx := context.Background()
	e.vehicle(t, "v1", 37.77, -122.42)
	id := e.ride(t, "k1", 37.7701, -122.4201)
	if err := e.svc.ProcessBatch(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if r := e.getRide(t, id); r.Status != store.RideMatching || r.VehicleID != nil {
		t.Fatalf("after timeout: %+v", r)
	}
	if v := e.getVehicle(t, "v1"); v.Status != store.VehicleAvailable || !e.inGeo(t, "v1") {
		t.Fatalf("vehicle after timeout: %s", v.Status)
	}
	var reason string
	if err := e.st.Pool().QueryRow(ctx, `SELECT reason FROM ride_rejections WHERE ride_id=$1`, id).Scan(&reason); err != nil || reason != "timeout" {
		t.Fatalf("rejection reason %q err %v", reason, err)
	}
}

func TestUnmatchedRideFails(t *testing.T) {
	e := setup(t, func(c *dispatch.Config) { c.MatchTimeout = 50 * time.Millisecond })
	ctx := context.Background()
	id := e.ride(t, "k1", 37.77, -122.42) // no vehicles
	if err := e.svc.ProcessBatch(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if err := e.svc.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if r := e.getRide(t, id); r.Status != store.RideFailed {
		t.Fatalf("status %s, want FAILED", r.Status)
	}
}

func TestTripLifecycleRecyclesVehicle(t *testing.T) {
	e := setup(t, nil)
	ctx := context.Background()
	e.vehicle(t, "v1", 37.77, -122.42)
	id := e.ride(t, "k1", 37.7701, -122.4201)
	if _, err := e.svc.Pickup(ctx, id, "v1"); err == nil {
		t.Fatal("pickup before dispatch must fail")
	}
	if err := e.svc.ProcessBatch(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	var te *store.TransitionError
	if _, err := e.svc.Pickup(ctx, id, "v1"); !errors.As(err, &te) {
		t.Fatalf("pickup before accept: %v", err)
	}
	if r, err := e.svc.Decide(ctx, id, "v1", true); err != nil || r.Status != store.RideAssigned || r.AssignedAt == nil {
		t.Fatalf("accept: %+v %v", r, err)
	}
	if v := e.getVehicle(t, "v1"); v.Status != store.VehicleEnRoute {
		t.Fatalf("vehicle %s", v.Status)
	}
	if _, err := e.svc.Pickup(ctx, id, "v2"); !errors.Is(err, store.ErrWrongVehicle) {
		t.Fatalf("wrong vehicle pickup: %v", err)
	}
	if r, err := e.svc.Pickup(ctx, id, "v1"); err != nil || r.Status != store.RideInTrip || r.PickedUpAt == nil {
		t.Fatalf("pickup: %+v %v", r, err)
	}
	if _, err := e.svc.Complete(ctx, id, "v2"); !errors.Is(err, store.ErrWrongVehicle) {
		t.Fatalf("wrong vehicle complete: %v", err)
	}
	if r, err := e.svc.Complete(ctx, id, "v1"); err != nil || r.Status != store.RideCompleted || r.CompletedAt == nil {
		t.Fatalf("complete: %+v %v", r, err)
	}
	if v := e.getVehicle(t, "v1"); v.Status != store.VehicleAvailable || !e.inGeo(t, "v1") {
		t.Fatalf("vehicle not recycled: %s", v.Status)
	}
	if _, err := e.svc.Complete(ctx, id, "v1"); !errors.As(err, &te) {
		t.Fatalf("second complete: %v", err)
	}
	// The recycled vehicle takes the next ride.
	id2 := e.ride(t, "k2", 37.7702, -122.4202)
	if err := e.svc.ProcessBatch(ctx, []string{id2}); err != nil {
		t.Fatal(err)
	}
	if r := e.getRide(t, id2); r.Status != store.RideDispatched {
		t.Fatalf("second ride %s", r.Status)
	}
	e.checkInvariants(t)
}

func TestStaleHeartbeatExcluded(t *testing.T) {
	e := setup(t, func(c *dispatch.Config) { c.HeartbeatMaxAge = 30 * time.Second })
	ctx := context.Background()
	v, err := e.st.CreateVehicle(ctx, store.Vehicle{ID: "stale", Status: store.VehicleAvailable, Lat: 37.77, Lng: -122.42, BatteryPct: 90})
	if err != nil {
		t.Fatal(err)
	}
	v.LastHeartbeatAt = time.Now().Add(-time.Minute)
	if err := e.c.UpsertVehicle(ctx, v); err != nil {
		t.Fatal(err)
	}
	id := e.ride(t, "k1", 37.7701, -122.4201)
	if err := e.svc.ProcessBatch(ctx, []string{id}); err != nil {
		t.Fatal(err)
	}
	if r := e.getRide(t, id); r.Status != store.RideMatching {
		t.Fatalf("stale vehicle was dispatched: %s", r.Status)
	}
}

func TestEnqueueDedupesByRideID(t *testing.T) {
	e := setup(t, nil)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := e.svc.Enqueue(ctx, "same"); err != nil {
			t.Fatal(err)
		}
	}
	if d := e.svc.QueueDepth(); d != 1 {
		t.Fatalf("queue depth %d, want 1", d)
	}
}

func TestRunDispatchesEndToEnd(t *testing.T) {
	e := setup(t, func(c *dispatch.Config) {
		c.BatchWindow = 50 * time.Millisecond
		c.RetryDelay = 50 * time.Millisecond
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	id := e.ride(t, "k1", 37.7701, -122.4201)
	done := make(chan struct{})
	go func() { e.svc.Run(ctx); close(done) }() // recovery scan enqueues the open ride
	time.Sleep(150 * time.Millisecond)
	e.vehicle(t, "v1", 37.77, -122.42) // arrives after the first attempt
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e.getRide(t, id).Status == store.RideDispatched {
			cancel()
			<-done
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("ride not dispatched: %s", e.getRide(t, id).Status)
}
