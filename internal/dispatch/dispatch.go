// Package dispatch matches rides to vehicles in batches.
//
// Ride IDs arrive on an in-process queue (from the outbox publisher or a
// requeue). Workers collect a batch window, search Redis GEO for candidates,
// price every candidate pair with a road-network ETA, solve the assignment
// with the Hungarian algorithm, and commit each pair under a vehicle lock in
// a single Postgres transaction.
package dispatch

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/yulo-dev/robotaxi-dispatch/internal/assign"
	"github.com/yulo-dev/robotaxi-dispatch/internal/cache"
	"github.com/yulo-dev/robotaxi-dispatch/internal/geo"
	"github.com/yulo-dev/robotaxi-dispatch/internal/store"
)

// Config tunes the dispatcher.
type Config struct {
	BatchWindow     time.Duration
	BatchMax        int
	RadiusM         float64
	ExpandM         float64
	TopK            int
	HeartbeatMaxAge time.Duration
	DecisionTimeout time.Duration
	MatchTimeout    time.Duration
	MaxHungarian    int
	LockTTL         time.Duration
	RetryDelay      time.Duration // wait before retrying a ride that found no vehicle
	SweepInterval   time.Duration
	Workers         int
	QueueSize       int
}

// DefaultConfig matches the service defaults.
func DefaultConfig() Config {
	return Config{
		BatchWindow: 2 * time.Second, BatchMax: 50,
		RadiusM: 3000, ExpandM: 6000, TopK: 10,
		HeartbeatMaxAge: 30 * time.Second, DecisionTimeout: 15 * time.Second,
		MatchTimeout: 120 * time.Second, MaxHungarian: 200, LockTTL: 5 * time.Second,
		RetryDelay: time.Second, SweepInterval: time.Second, Workers: 2, QueueSize: 10_000,
	}
}

// Observer receives dispatcher events for metrics.
type Observer interface {
	RideAssigned(timeToAssign time.Duration)
	RideFailed(n int)
	Rejection()
	AssignmentConflict()
	LockContention()
	BatchSize(n int)
	HungarianSolve(d time.Duration)
	QueueDepth(n int)
}

type nopObserver struct{}

func (nopObserver) RideAssigned(time.Duration)   {}
func (nopObserver) RideFailed(int)               {}
func (nopObserver) Rejection()                   {}
func (nopObserver) AssignmentConflict()          {}
func (nopObserver) LockContention()              {}
func (nopObserver) BatchSize(int)                {}
func (nopObserver) HungarianSolve(time.Duration) {}
func (nopObserver) QueueDepth(int)               {}

// Service is the dispatcher.
type Service struct {
	cfg    Config
	store  *store.Store
	cache  *cache.Cache
	router *geo.Router
	obs    Observer
	log    *slog.Logger
	now    func() time.Time

	queue   chan string
	mu      sync.Mutex
	pending map[string]bool // ride IDs currently queued (dedupe)
}

// New builds a dispatcher. obs and log may be nil.
func New(cfg Config, st *store.Store, c *cache.Cache, router *geo.Router, obs Observer, log *slog.Logger) *Service {
	if obs == nil {
		obs = nopObserver{}
	}
	if log == nil {
		log = slog.Default()
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.QueueSize < 1 {
		cfg.QueueSize = 1
	}
	return &Service{
		cfg: cfg, store: st, cache: c, router: router, obs: obs, log: log, now: time.Now,
		queue: make(chan string, cfg.QueueSize), pending: map[string]bool{},
	}
}

// Enqueue adds a ride ID to the dispatch queue. A ride already queued is
// dropped, so at-least-once delivery from the outbox does not create
// duplicate work. It blocks while the queue is full.
func (s *Service) Enqueue(ctx context.Context, rideID string) error {
	s.mu.Lock()
	if s.pending[rideID] {
		s.mu.Unlock()
		return nil
	}
	s.pending[rideID] = true
	s.mu.Unlock()
	select {
	case s.queue <- rideID:
		s.obs.QueueDepth(len(s.queue))
		return nil
	case <-ctx.Done():
		s.mu.Lock()
		delete(s.pending, rideID)
		s.mu.Unlock()
		return ctx.Err()
	}
}

// QueueDepth returns the number of queued rides.
func (s *Service) QueueDepth() int { return len(s.queue) }

func (s *Service) requeueLater(ctx context.Context, rideID string, after time.Duration) {
	time.AfterFunc(after, func() {
		if ctx.Err() == nil {
			_ = s.Enqueue(ctx, rideID)
		}
	})
}

// Run starts workers and the timeout sweeper and blocks until ctx ends. On
// start it enqueues rides that were waiting for a vehicle, which recovers
// work lost if a previous process stopped between publish and dispatch.
func (s *Service) Run(ctx context.Context) {
	if ids, err := s.store.OpenRideIDs(ctx, 10_000); err == nil {
		for _, id := range ids {
			if s.Enqueue(ctx, id) != nil {
				break
			}
		}
	} else if ctx.Err() == nil {
		s.log.Warn("recovery scan failed", "err", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < s.cfg.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.worker(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.sweeper(ctx)
	}()
	wg.Wait()
}

func (s *Service) worker(ctx context.Context) {
	for {
		batch, ok := s.collect(ctx)
		if !ok {
			return
		}
		if err := s.ProcessBatch(ctx, batch); err != nil && ctx.Err() == nil {
			s.log.Error("batch failed", "err", err, "size", len(batch))
			for _, id := range batch {
				s.requeueLater(ctx, id, s.cfg.RetryDelay)
			}
		}
	}
}

// collect waits for one ride, then gathers more until the batch window
// elapses or BatchMax is reached.
func (s *Service) collect(ctx context.Context) ([]string, bool) {
	var batch []string
	take := func(id string) {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
		batch = append(batch, id)
	}
	select {
	case id := <-s.queue:
		take(id)
	case <-ctx.Done():
		return nil, false
	}
	timer := time.NewTimer(s.cfg.BatchWindow)
	defer timer.Stop()
	for len(batch) < s.cfg.BatchMax {
		select {
		case id := <-s.queue:
			take(id)
		case <-timer.C:
			s.obs.QueueDepth(len(s.queue))
			return batch, true
		case <-ctx.Done():
			return batch, true
		}
	}
	s.obs.QueueDepth(len(s.queue))
	return batch, true
}

type workItem struct {
	ride  store.Ride
	lock  *cache.Lock
	cands []cache.Candidate
}

// ProcessBatch runs one dispatch round for the given ride IDs.
func (s *Service) ProcessBatch(ctx context.Context, ids []string) error {
	ids = dedupe(ids)
	s.obs.BatchSize(len(ids))

	// 1. Ride locks: one worker handles a ride at a time.
	locks := map[string]*cache.Lock{}
	defer func() {
		for _, l := range locks {
			_ = s.cache.Release(context.WithoutCancel(ctx), l)
		}
	}()
	var locked []string
	for _, id := range ids {
		l, err := s.cache.LockRide(ctx, id, s.cfg.LockTTL)
		if errors.Is(err, cache.ErrNotHeld) {
			s.obs.LockContention()
			continue // another worker owns it and will requeue if needed
		}
		if err != nil {
			return err
		}
		locks[id] = l
		locked = append(locked, id)
	}
	if len(locked) == 0 {
		return nil
	}

	// 2. Load rides; skip any that are no longer waiting (idempotent consumer).
	rides, err := s.store.GetRides(ctx, locked)
	if err != nil {
		return err
	}
	rejected, err := s.store.Rejections(ctx, locked)
	if err != nil {
		return err
	}
	now := s.now()
	var items []*workItem
	for _, r := range rides {
		if r.Status != store.RideRequested && r.Status != store.RideMatching {
			continue
		}
		if now.Sub(r.RequestedAt) >= s.cfg.MatchTimeout {
			continue // the sweeper fails it
		}
		r, err := s.store.StartMatching(ctx, r)
		if err != nil {
			if errors.Is(err, store.ErrConflict) {
				s.obs.AssignmentConflict()
				s.requeueLater(ctx, r.ID, s.cfg.RetryDelay)
				continue
			}
			return err
		}
		cands, err := s.cache.Candidates(ctx, cache.SearchQuery{
			Lat: r.PickupLat, Lng: r.PickupLng,
			RadiusM: s.cfg.RadiusM, ExpandM: s.cfg.ExpandM, K: s.cfg.TopK,
			MaxAge: s.cfg.HeartbeatMaxAge, Exclude: rejected[r.ID], Now: now,
		})
		if err != nil {
			return err
		}
		items = append(items, &workItem{ride: r, lock: locks[r.ID], cands: cands})
	}

	// 3. Cost matrix over the union of candidates: road ETA, +Inf if the
	// vehicle is not a candidate for the ride or the route does not exist.
	colOf := map[string]int{}
	var cols []cache.Candidate
	for _, it := range items {
		for _, c := range it.cands {
			if _, ok := colOf[c.ID]; !ok {
				colOf[c.ID] = len(cols)
				cols = append(cols, c)
			}
		}
	}
	cost := make([][]float64, len(items))
	for i, it := range items {
		row := make([]float64, len(cols))
		for j := range row {
			row[j] = math.Inf(1)
		}
		pickup := geo.Location{Lat: it.ride.PickupLat, Lng: it.ride.PickupLng}
		for _, c := range it.cands {
			if eta, err := s.router.ETA(geo.Location{Lat: c.Lat, Lng: c.Lng}, pickup); err == nil {
				row[colOf[c.ID]] = eta
			}
		}
		cost[i] = row
	}
	start := time.Now()
	solution, _ := assign.Solve(cost, s.cfg.MaxHungarian)
	s.obs.HungarianSolve(time.Since(start))

	// 4. Commit each pair.
	for i, col := range solution {
		it := items[i]
		if col < 0 {
			s.requeueLater(ctx, it.ride.ID, s.cfg.RetryDelay)
			continue
		}
		if err := s.commitPair(ctx, it.ride, cols[col].ID); err != nil {
			if !errors.Is(err, errRetry) {
				return err
			}
			s.requeueLater(ctx, it.ride.ID, s.cfg.RetryDelay)
		}
	}
	return nil
}

var errRetry = errors.New("retry")

func (s *Service) commitPair(ctx context.Context, ride store.Ride, vehicleID string) error {
	vl, err := s.cache.LockVehicle(ctx, vehicleID, s.cfg.LockTTL)
	if errors.Is(err, cache.ErrNotHeld) {
		s.obs.LockContention()
		return errRetry
	}
	if err != nil {
		return err
	}
	defer func() { _ = s.cache.Release(context.WithoutCancel(ctx), vl) }()

	_, veh, err := s.store.DispatchRide(ctx, ride.ID, ride.Version, vehicleID)
	if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrVehicleBusy) {
		s.obs.AssignmentConflict()
		return errRetry
	}
	if err != nil {
		return err
	}
	// Removes the vehicle from vehicles:geo and records its ride.
	if err := s.cache.UpsertVehicleRide(ctx, veh, ride.ID); err != nil {
		s.log.Warn("cache update after dispatch failed", "vehicle", vehicleID, "err", err)
	}
	s.log.Info("ride dispatched", "ride", ride.ID, "vehicle", vehicleID)
	return nil
}

func dedupe(ids []string) []string {
	seen := map[string]bool{}
	out := ids[:0:0]
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out
}

// ---- vehicle decisions and trip lifecycle ----

// Decide applies a vehicle's ACCEPT or REJECT for a dispatched ride.
func (s *Service) Decide(ctx context.Context, rideID, vehicleID string, accept bool) (store.Ride, error) {
	if accept {
		ride, veh, err := s.store.AcceptRide(ctx, rideID, vehicleID)
		if err != nil {
			return ride, err
		}
		s.syncVehicle(ctx, veh, rideID)
		s.obs.RideAssigned(s.now().Sub(ride.RequestedAt))
		return ride, nil
	}
	return s.release(ctx, rideID, vehicleID, store.ReasonRejected)
}

func (s *Service) release(ctx context.Context, rideID, vehicleID, reason string) (store.Ride, error) {
	ride, veh, err := s.store.RejectRide(ctx, rideID, vehicleID, reason)
	if err != nil {
		return ride, err
	}
	s.syncVehicle(ctx, veh, "")
	s.obs.Rejection()
	// The outbox row written with the rejection also requeues the ride; this
	// direct enqueue only avoids waiting for the next publisher poll.
	_ = s.Enqueue(ctx, rideID)
	return ride, nil
}

// Pickup records that the vehicle picked up the rider.
func (s *Service) Pickup(ctx context.Context, rideID, vehicleID string) (store.Ride, error) {
	ride, veh, err := s.store.PickupRide(ctx, rideID, vehicleID)
	if err == nil {
		s.syncVehicle(ctx, veh, rideID)
	}
	return ride, err
}

// Complete finishes the trip and returns the vehicle to the available pool.
func (s *Service) Complete(ctx context.Context, rideID, vehicleID string) (store.Ride, error) {
	ride, veh, err := s.store.CompleteRide(ctx, rideID, vehicleID)
	if err == nil {
		s.syncVehicle(ctx, veh, "")
	}
	return ride, err
}

func (s *Service) syncVehicle(ctx context.Context, v store.Vehicle, rideID string) {
	if err := s.cache.UpsertVehicleRide(ctx, v, rideID); err != nil {
		s.log.Warn("cache update failed", "vehicle", v.ID, "err", err)
	}
}

// ---- timeouts ----

func (s *Service) sweeper(ctx context.Context) {
	t := time.NewTicker(s.cfg.SweepInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.Sweep(ctx); err != nil && ctx.Err() == nil {
				s.log.Warn("sweep failed", "err", err)
			}
		}
	}
}

// Sweep handles timeouts: dispatched rides with no answer return to
// MATCHING, and rides waiting longer than MatchTimeout become FAILED.
func (s *Service) Sweep(ctx context.Context) error {
	now := s.now()
	expired, err := s.store.DispatchedBefore(ctx, now.Add(-s.cfg.DecisionTimeout), 500)
	if err != nil {
		return err
	}
	for _, e := range expired {
		_, err := s.release(ctx, e.RideID, e.VehicleID, store.ReasonTimeout)
		var te *store.TransitionError
		if err != nil && !errors.As(err, &te) && !errors.Is(err, store.ErrWrongVehicle) {
			return err // a concurrent decision is fine; anything else is not
		}
	}
	failed, err := s.store.FailUnmatched(ctx, now.Add(-s.cfg.MatchTimeout))
	if err != nil {
		return err
	}
	if len(failed) > 0 {
		s.obs.RideFailed(len(failed))
		s.log.Info("rides failed", "count", len(failed))
	}
	return nil
}
