package store

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// Rides with an active vehicle are guarded by this partial unique index.
const activeRideIndex = "uniq_active_ride_per_vehicle"

// GetRides loads several rides by ID. Missing IDs are omitted.
func (s *Store) GetRides(ctx context.Context, ids []string) ([]Ride, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+rideCols+` FROM rides WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Ride
	for rows.Next() {
		r, err := scanRide(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StartMatching moves a REQUESTED ride to MATCHING. A ride already in
// MATCHING is returned unchanged.
func (s *Store) StartMatching(ctx context.Context, r Ride) (Ride, error) {
	if r.Status == RideMatching {
		return r, nil
	}
	if r.Status != RideRequested {
		return r, &TransitionError{From: r.Status, To: RideMatching}
	}
	out, err := scanRide(s.pool.QueryRow(ctx, `UPDATE rides SET status='MATCHING', version=version+1
		WHERE id=$1 AND version=$2 AND status='REQUESTED' RETURNING `+rideCols, r.ID, r.Version))
	if errors.Is(err, ErrNotFound) {
		return r, ErrConflict
	}
	return out, err
}

// Rejections returns, per ride, the vehicles excluded from it.
func (s *Store) Rejections(ctx context.Context, rideIDs []string) (map[string]map[string]bool, error) {
	rows, err := s.pool.Query(ctx, `SELECT ride_id::text, vehicle_id FROM ride_rejections WHERE ride_id = ANY($1::uuid[])`, rideIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]bool{}
	for rows.Next() {
		var r, v string
		if err := rows.Scan(&r, &v); err != nil {
			return nil, err
		}
		if out[r] == nil {
			out[r] = map[string]bool{}
		}
		out[r][v] = true
	}
	return out, rows.Err()
}

// DispatchRide binds a MATCHING ride to an AVAILABLE vehicle in one
// transaction. The ride update is conditional on the version the caller read
// (optimistic locking); the vehicle update is conditional on it being
// AVAILABLE; the partial unique index rejects a second active ride for the
// vehicle. Any of these failing returns ErrConflict or ErrVehicleBusy and
// changes nothing.
func (s *Store) DispatchRide(ctx context.Context, rideID string, rideVersion int64, vehicleID string) (Ride, Vehicle, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ride{}, Vehicle{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := s.now()
	ride, err := scanRide(tx.QueryRow(ctx, `UPDATE rides SET status='DISPATCHED', vehicle_id=$3,
			dispatched_at=$4, version=version+1
		WHERE id=$1 AND version=$2 AND status='MATCHING' RETURNING `+rideCols,
		rideID, rideVersion, vehicleID, now))
	if errors.Is(err, ErrNotFound) {
		return Ride{}, Vehicle{}, ErrConflict
	}
	if isUniqueViolation(err, activeRideIndex) {
		return Ride{}, Vehicle{}, ErrVehicleBusy
	}
	if err != nil {
		return Ride{}, Vehicle{}, err
	}
	veh, err := scanVehicle(tx.QueryRow(ctx, `UPDATE vehicles SET status='DISPATCHED', version=version+1
		WHERE id=$1 AND status='AVAILABLE' RETURNING `+vehicleCols, vehicleID))
	if errors.Is(err, ErrNotFound) {
		return Ride{}, Vehicle{}, ErrVehicleBusy
	}
	if err != nil {
		return Ride{}, Vehicle{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err, activeRideIndex) {
			return Ride{}, Vehicle{}, ErrVehicleBusy
		}
		return Ride{}, Vehicle{}, err
	}
	return ride, veh, nil
}

// ErrWrongVehicle means the vehicle in the request is not the ride's vehicle.
var ErrWrongVehicle = errors.New("vehicle is not assigned to this ride")

// lifecycleStep moves a ride bound to vehicleID from one status to another
// and sets the vehicle status, in one transaction.
// rideSet is extra SET clauses; it may reference $3 as the current time.
func (s *Store) lifecycleStep(ctx context.Context, rideID, vehicleID string, to RideStatus, rideSet string,
	vehicleStatus VehicleStatus, extra func(ctx context.Context, tx pgx.Tx) error) (Ride, Vehicle, error) {
	if !validUUID(rideID) {
		return Ride{}, Vehicle{}, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ride{}, Vehicle{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	cur, err := scanRide(tx.QueryRow(ctx, `SELECT `+rideCols+` FROM rides WHERE id=$1 FOR UPDATE`, rideID))
	if err != nil {
		return Ride{}, Vehicle{}, err
	}
	if !CanTransition(cur.Status, to) {
		return cur, Vehicle{}, &TransitionError{From: cur.Status, To: to}
	}
	if cur.VehicleID == nil || *cur.VehicleID != vehicleID {
		return cur, Vehicle{}, ErrWrongVehicle
	}
	ride, err := scanRide(tx.QueryRow(ctx, `UPDATE rides SET status=$2, `+rideSet+`, version=version+1
		WHERE id=$1 AND $3::timestamptz IS NOT NULL RETURNING `+rideCols, rideID, to, s.now()))
	if err != nil {
		return Ride{}, Vehicle{}, err
	}
	veh, err := scanVehicle(tx.QueryRow(ctx, `UPDATE vehicles SET status=$2, version=version+1
		WHERE id=$1 RETURNING `+vehicleCols, vehicleID, vehicleStatus))
	if err != nil {
		return Ride{}, Vehicle{}, err
	}
	if extra != nil {
		if err := extra(ctx, tx); err != nil {
			return Ride{}, Vehicle{}, err
		}
	}
	return ride, veh, tx.Commit(ctx)
}

// AcceptRide: DISPATCHED -> ASSIGNED, vehicle -> EN_ROUTE.
func (s *Store) AcceptRide(ctx context.Context, rideID, vehicleID string) (Ride, Vehicle, error) {
	return s.lifecycleStep(ctx, rideID, vehicleID, RideAssigned, "assigned_at=$3", VehicleEnRoute, nil)
}

// PickupRide: ASSIGNED -> IN_TRIP, vehicle -> IN_TRIP.
func (s *Store) PickupRide(ctx context.Context, rideID, vehicleID string) (Ride, Vehicle, error) {
	return s.lifecycleStep(ctx, rideID, vehicleID, RideInTrip, "picked_up_at=$3", VehicleInTrip, nil)
}

// CompleteRide: IN_TRIP -> COMPLETED, vehicle -> AVAILABLE.
func (s *Store) CompleteRide(ctx context.Context, rideID, vehicleID string) (Ride, Vehicle, error) {
	return s.lifecycleStep(ctx, rideID, vehicleID, RideCompleted, "completed_at=$3", VehicleAvailable, nil)
}

// Rejection reasons.
const (
	ReasonRejected = "rejected"
	ReasonTimeout  = "timeout"
)

// RejectRide: DISPATCHED -> MATCHING, vehicle -> AVAILABLE, and the vehicle
// is excluded from this ride from now on. The requeue event is written to the
// outbox in the same transaction so a crash cannot strand the ride.
func (s *Store) RejectRide(ctx context.Context, rideID, vehicleID, reason string) (Ride, Vehicle, error) {
	return s.lifecycleStep(ctx, rideID, vehicleID, RideMatching,
		"vehicle_id=NULL, dispatched_at=NULL",
		VehicleAvailable, func(ctx context.Context, tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, `INSERT INTO ride_rejections (ride_id, vehicle_id, reason) VALUES ($1, $2, $3)
				ON CONFLICT DO NOTHING`, rideID, vehicleID, reason); err != nil {
				return err
			}
			return insertOutbox(ctx, tx, rideID, EventRideRequeued)
		})
}

// ExpiredDispatch is a ride whose vehicle did not answer in time.
type ExpiredDispatch struct {
	RideID, VehicleID string
}

// DispatchedBefore lists DISPATCHED rides offered before cutoff.
func (s *Store) DispatchedBefore(ctx context.Context, cutoff time.Time, limit int) ([]ExpiredDispatch, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text, vehicle_id FROM rides
		WHERE status='DISPATCHED' AND dispatched_at < $1 ORDER BY dispatched_at LIMIT $2`, cutoff, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ExpiredDispatch
	for rows.Next() {
		var e ExpiredDispatch
		if err := rows.Scan(&e.RideID, &e.VehicleID); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// FailUnmatched marks REQUESTED/MATCHING rides requested before cutoff as
// FAILED and returns their IDs.
func (s *Store) FailUnmatched(ctx context.Context, cutoff time.Time) ([]string, error) {
	rows, err := s.pool.Query(ctx, `UPDATE rides SET status='FAILED', version=version+1
		WHERE status IN ('REQUESTED','MATCHING') AND requested_at < $1 RETURNING id::text`, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// OpenRideIDs lists rides waiting for a vehicle (REQUESTED or MATCHING).
func (s *Store) OpenRideIDs(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM rides WHERE status IN ('REQUESTED','MATCHING')
		ORDER BY requested_at LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}
