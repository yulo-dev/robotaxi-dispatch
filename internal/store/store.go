package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yulo-dev/robotaxi-dispatch/migrations"
)

// Store wraps a pgx connection pool.
type Store struct {
	pool *pgxpool.Pool
	now  func() time.Time
}

// Open connects to Postgres.
func Open(ctx context.Context, url string, maxConns int32) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	if maxConns > 0 {
		cfg.MaxConns = maxConns
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	return &Store{pool: pool, now: time.Now}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.pool.Close() }

// Ping checks connectivity.
func (s *Store) Ping(ctx context.Context) error { return s.pool.Ping(ctx) }

// Pool exposes the pool for tests and the outbox publisher.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

const migrateLockID = 7_440_001

// Migrate applies embedded SQL migrations that have not run yet. A session
// advisory lock serializes concurrent callers.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrateLockID); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrateLockID) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var done bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func isUniqueViolation(err error, constraint string) bool {
	var pg *pgconn.PgError
	if errors.As(err, &pg) && pg.Code == "23505" {
		return constraint == "" || pg.ConstraintName == constraint
	}
	return false
}

// ---- vehicles ----

const vehicleCols = `id, status, lat, lng, battery_pct, last_heartbeat_at, version`

func scanVehicle(row pgx.Row) (Vehicle, error) {
	var v Vehicle
	var battery float32
	err := row.Scan(&v.ID, &v.Status, &v.Lat, &v.Lng, &battery, &v.LastHeartbeatAt, &v.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return v, ErrNotFound
	}
	v.BatteryPct = float64(battery)
	return v, err
}

// CreateVehicle inserts a vehicle. Returns ErrExists if the ID is taken.
func (s *Store) CreateVehicle(ctx context.Context, v Vehicle) (Vehicle, error) {
	row := s.pool.QueryRow(ctx, `INSERT INTO vehicles (id, status, lat, lng, battery_pct, last_heartbeat_at)
		VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+vehicleCols,
		v.ID, v.Status, v.Lat, v.Lng, v.BatteryPct, s.now())
	out, err := scanVehicle(row)
	if isUniqueViolation(err, "") {
		return Vehicle{}, ErrExists
	}
	return out, err
}

// GetVehicle loads a vehicle.
func (s *Store) GetVehicle(ctx context.Context, id string) (Vehicle, error) {
	return scanVehicle(s.pool.QueryRow(ctx, `SELECT `+vehicleCols+` FROM vehicles WHERE id=$1`, id))
}

// ListVehicles returns vehicles ordered by ID, optionally filtered by status.
func (s *Store) ListVehicles(ctx context.Context, status VehicleStatus, limit int) ([]Vehicle, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := s.pool.Query(ctx, `SELECT `+vehicleCols+` FROM vehicles
		WHERE ($1 = '' OR status = $1) ORDER BY id LIMIT $2`, string(status), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Vehicle{}
	for rows.Next() {
		v, err := scanVehicle(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Heartbeat updates location and battery. A reported status is applied only
// when it is AVAILABLE or OFFLINE and the vehicle is not bound to a ride;
// ride-bound statuses are owned by the dispatcher.
func (s *Store) Heartbeat(ctx context.Context, id string, lat, lng, battery float64, status VehicleStatus) (Vehicle, error) {
	return scanVehicle(s.pool.QueryRow(ctx, `UPDATE vehicles SET
			lat=$2, lng=$3, battery_pct=$4, last_heartbeat_at=$6,
			status = CASE WHEN $5 IN ('AVAILABLE','OFFLINE')
			              AND status NOT IN ('DISPATCHED','EN_ROUTE','IN_TRIP')
			         THEN $5 ELSE status END,
			version = version + 1
		WHERE id=$1 RETURNING `+vehicleCols, id, lat, lng, battery, string(status), s.now()))
}

// ---- rides ----

const rideCols = `id, rider_id, pickup_lat, pickup_lng, dropoff_lat, dropoff_lng, status, vehicle_id,
	requested_at, assigned_at, picked_up_at, completed_at, version`

func scanRide(row pgx.Row) (Ride, error) {
	var r Ride
	var id uuid.UUID
	err := row.Scan(&id, &r.RiderID, &r.PickupLat, &r.PickupLng, &r.DropoffLat, &r.DropoffLng, &r.Status,
		&r.VehicleID, &r.RequestedAt, &r.AssignedAt, &r.PickedUpAt, &r.CompletedAt, &r.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, ErrNotFound
	}
	r.ID = id.String()
	return r, err
}

// NewRide describes a ride to create.
type NewRide struct {
	RiderID    string
	PickupLat  float64
	PickupLng  float64
	DropoffLat float64
	DropoffLng float64
}

// StoredResponse is a response recorded under an idempotency key.
type StoredResponse struct {
	Status int
	Body   json.RawMessage
}

// Event types written to the outbox.
const (
	EventRideRequested = "ride.requested"
	EventRideRequeued  = "ride.requeued"
)

// CreateRide creates a ride and its ride.requested outbox event in one
// transaction, recording the response under the idempotency key.
//
// The key row is inserted first with ON CONFLICT DO NOTHING. A concurrent
// request with the same key blocks on the primary key until the first
// transaction commits, then sees the conflict and replays the stored
// response. A key reused with a different request hash returns
// ErrIdempotencyReuse. replayed is true when the response came from storage.
func (s *Store) CreateRide(ctx context.Context, key, requestHash string, nr NewRide,
	render func(Ride) StoredResponse) (resp StoredResponse, replayed bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return resp, false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	ride := Ride{
		ID: uuid.NewString(), RiderID: nr.RiderID,
		PickupLat: nr.PickupLat, PickupLng: nr.PickupLng,
		DropoffLat: nr.DropoffLat, DropoffLng: nr.DropoffLng,
		Status: RideRequested, RequestedAt: s.now().UTC().Truncate(time.Microsecond),
	}
	resp = render(ride)
	tag, err := tx.Exec(ctx, `INSERT INTO idempotency_keys (key, request_hash, response_status, response_body)
		VALUES ($1, $2, $3, $4) ON CONFLICT (key) DO NOTHING`, key, requestHash, resp.Status, []byte(resp.Body))
	if err != nil {
		return resp, false, err
	}
	if tag.RowsAffected() == 0 {
		_ = tx.Rollback(ctx)
		var hash string
		var stored StoredResponse
		var body []byte
		if err := s.pool.QueryRow(ctx, `SELECT request_hash, response_status, response_body
			FROM idempotency_keys WHERE key=$1`, key).Scan(&hash, &stored.Status, &body); err != nil {
			return resp, false, err
		}
		if hash != requestHash {
			return StoredResponse{}, false, ErrIdempotencyReuse
		}
		stored.Body = body
		return stored, true, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO rides (id, rider_id, pickup_lat, pickup_lng, dropoff_lat, dropoff_lng,
		status, requested_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		ride.ID, ride.RiderID, ride.PickupLat, ride.PickupLng, ride.DropoffLat, ride.DropoffLng,
		ride.Status, ride.RequestedAt); err != nil {
		return resp, false, err
	}
	if err := insertOutbox(ctx, tx, ride.ID, EventRideRequested); err != nil {
		return resp, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return resp, false, err
	}
	return resp, false, nil
}

func insertOutbox(ctx context.Context, tx pgx.Tx, rideID, eventType string) error {
	payload, _ := json.Marshal(map[string]string{"ride_id": rideID})
	_, err := tx.Exec(ctx, `INSERT INTO outbox_events (aggregate_id, event_type, payload) VALUES ($1, $2, $3)`,
		rideID, eventType, payload)
	return err
}

// GetRide loads a ride.
func (s *Store) GetRide(ctx context.Context, id string) (Ride, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Ride{}, ErrNotFound
	}
	return scanRide(s.pool.QueryRow(ctx, `SELECT `+rideCols+` FROM rides WHERE id=$1`, id))
}

// CancelRide moves a ride to CANCELLED if the state machine allows it and
// frees its vehicle. Returns the updated ride and the freed vehicle ID ("" if none).
func (s *Store) CancelRide(ctx context.Context, id string) (Ride, string, error) {
	if _, err := uuid.Parse(id); err != nil {
		return Ride{}, "", ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Ride{}, "", err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	cur, err := scanRide(tx.QueryRow(ctx, `SELECT `+rideCols+` FROM rides WHERE id=$1 FOR UPDATE`, id))
	if err != nil {
		return Ride{}, "", err
	}
	if !CanTransition(cur.Status, RideCancelled) {
		return cur, "", &TransitionError{From: cur.Status, To: RideCancelled}
	}
	out, err := scanRide(tx.QueryRow(ctx, `UPDATE rides SET status=$2, version=version+1
		WHERE id=$1 RETURNING `+rideCols, id, RideCancelled))
	if err != nil {
		return Ride{}, "", err
	}
	freed := ""
	if cur.VehicleID != nil {
		freed = *cur.VehicleID
		if _, err := tx.Exec(ctx, `UPDATE vehicles SET status='AVAILABLE', version=version+1
			WHERE id=$1 AND status IN ('DISPATCHED','EN_ROUTE','IN_TRIP')`, freed); err != nil {
			return Ride{}, "", err
		}
	}
	return out, freed, tx.Commit(ctx)
}

// DeleteExpiredIdempotencyKeys removes keys older than ttl.
func (s *Store) DeleteExpiredIdempotencyKeys(ctx context.Context, ttl time.Duration) (int64, error) {
	tag, err := s.pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < $1`, s.now().Add(-ttl))
	return tag.RowsAffected(), err
}
