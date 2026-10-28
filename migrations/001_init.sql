CREATE TABLE vehicles (
  id TEXT PRIMARY KEY,
  status TEXT NOT NULL,
  lat DOUBLE PRECISION NOT NULL,
  lng DOUBLE PRECISION NOT NULL,
  battery_pct REAL NOT NULL,
  last_heartbeat_at TIMESTAMPTZ NOT NULL,
  version BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE rides (
  id UUID PRIMARY KEY,
  rider_id TEXT NOT NULL,
  pickup_lat DOUBLE PRECISION NOT NULL, pickup_lng DOUBLE PRECISION NOT NULL,
  dropoff_lat DOUBLE PRECISION NOT NULL, dropoff_lng DOUBLE PRECISION NOT NULL,
  status TEXT NOT NULL,
  vehicle_id TEXT REFERENCES vehicles(id),
  requested_at TIMESTAMPTZ NOT NULL,
  assigned_at TIMESTAMPTZ, picked_up_at TIMESTAMPTZ, completed_at TIMESTAMPTZ,
  version BIGINT NOT NULL DEFAULT 0
);

-- One active ride per vehicle
CREATE UNIQUE INDEX uniq_active_ride_per_vehicle
  ON rides(vehicle_id) WHERE status IN ('DISPATCHED','ASSIGNED','IN_TRIP');

CREATE TABLE outbox_events (
  id BIGSERIAL PRIMARY KEY,
  aggregate_id TEXT NOT NULL,
  event_type TEXT NOT NULL,
  payload JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  published_at TIMESTAMPTZ,
  attempts INT NOT NULL DEFAULT 0
);
CREATE INDEX outbox_unpublished ON outbox_events(id) WHERE published_at IS NULL;

CREATE TABLE idempotency_keys (
  key TEXT PRIMARY KEY,
  request_hash TEXT NOT NULL,
  response_status INT NOT NULL,
  response_body JSONB NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
