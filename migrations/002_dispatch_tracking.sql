-- When the current vehicle was dispatched (used for the decision timeout).
ALTER TABLE rides ADD COLUMN dispatched_at TIMESTAMPTZ;

-- Vehicles that rejected or timed out on a ride; excluded on later attempts.
CREATE TABLE ride_rejections (
  ride_id UUID NOT NULL REFERENCES rides(id),
  vehicle_id TEXT NOT NULL REFERENCES vehicles(id),
  reason TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (ride_id, vehicle_id)
);

CREATE INDEX rides_open_by_status ON rides(status, requested_at)
  WHERE status IN ('REQUESTED','MATCHING','DISPATCHED');
