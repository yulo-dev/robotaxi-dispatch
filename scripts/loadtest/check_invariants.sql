-- Run after a load test. Every row must show ok = true.
\pset footer off
SELECT 'no vehicle has two active rides' AS invariant,
       count(*) AS violations, count(*) = 0 AS ok
FROM (SELECT vehicle_id FROM rides
      WHERE status IN ('DISPATCHED','ASSIGNED','IN_TRIP')
      GROUP BY vehicle_id HAVING count(*) > 1) d
UNION ALL
SELECT 'no ride stored under two idempotency keys',
       count(*), count(*) = 0
FROM (SELECT response_body->>'id' FROM idempotency_keys
      GROUP BY 1 HAVING count(*) > 1) d
UNION ALL
SELECT 'one ride per idempotency key (rides - keys)',
       abs((SELECT count(*) FROM rides) - (SELECT count(*) FROM idempotency_keys)),
       (SELECT count(*) FROM rides) = (SELECT count(*) FROM idempotency_keys)
UNION ALL
SELECT 'every ride has a ride.requested outbox event',
       count(*), count(*) = 0
FROM rides r
WHERE NOT EXISTS (SELECT 1 FROM outbox_events o
                  WHERE o.aggregate_id = r.id::text AND o.event_type = 'ride.requested')
UNION ALL
SELECT 'busy vehicles match active rides',
       count(*), count(*) = 0
FROM vehicles v
WHERE (v.status IN ('DISPATCHED','EN_ROUTE','IN_TRIP')) <>
      EXISTS (SELECT 1 FROM rides r WHERE r.vehicle_id = v.id
              AND r.status IN ('DISPATCHED','ASSIGNED','IN_TRIP'));

SELECT status, count(*) AS rides FROM rides GROUP BY status ORDER BY status;
