# Design notes

This document explains how a ride moves through the system and why each guard exists. The README has the summary tables; this file has the reasoning.

## Ride lifecycle

```
REQUESTED -> MATCHING -> DISPATCHED -> ASSIGNED -> IN_TRIP -> COMPLETED
                 |            |
                 |            +--(vehicle rejects / 15 s timeout)--> MATCHING
                 +--(still unmatched 120 s after request)--> FAILED
REQUESTED/MATCHING/DISPATCHED/ASSIGNED --(rider cancels)--> CANCELLED
```

`REQUESTED -> FAILED` is also allowed so a ride that never reached a dispatcher still expires. Lifecycle steps (accept, reject, pickup, complete, cancel) lock the ride row with `SELECT ... FOR UPDATE` and check `store.CanTransition` in the same transaction. Dispatcher steps (start matching, dispatch, fail) make the update conditional on the current status, and the dispatch update also on the version read. Illegal transitions return HTTP 409.

| step | who | transaction |
|---|---|---|
| create | API | idempotency key row, ride `REQUESTED`, outbox `ride.requested` |
| publish | outbox publisher | lock unpublished rows (`SKIP LOCKED`), hand ride IDs to the queue, mark published |
| match | dispatcher | ride `MATCHING` (version-checked) |
| dispatch | dispatcher | ride `DISPATCHED` + `vehicle_id` + `dispatched_at` (version-checked), vehicle `DISPATCHED` (only if `AVAILABLE`) |
| accept | vehicle via API | ride `ASSIGNED` + `assigned_at`, vehicle `EN_ROUTE` |
| reject or timeout | vehicle via API, or sweeper | ride `MATCHING`, `vehicle_id` cleared, `ride_rejections` row, vehicle `AVAILABLE`, outbox `ride.requeued` |
| pickup | vehicle via API | ride `IN_TRIP` + `picked_up_at`, vehicle `IN_TRIP` |
| complete | vehicle via API | ride `COMPLETED` + `completed_at`, vehicle `AVAILABLE` |
| cancel | rider via API | ride `CANCELLED`, vehicle `AVAILABLE` if one was bound |

Pickup and complete require the `vehicle_id` in the body to match the ride's vehicle. After every vehicle transition the dispatcher mirrors the vehicle into Redis, which also adds it to or removes it from `vehicles:geo` and records or clears its active ride.

`migrations/002_dispatch_tracking.sql` adds `rides.dispatched_at` (the decision timeout needs to know when the current vehicle was offered the ride) and `ride_rejections` (vehicles excluded from a ride after rejecting or timing out). Both live in Postgres so the exclusion and the timeout survive a restart.

## Idempotent ride creation

`POST /v1/rides` requires an `Idempotency-Key`. The middleware hashes the canonical JSON form of the body (sorted keys, no whitespace, numbers kept as written), so a retry with reordered fields still matches.

The ride transaction inserts the key row first with `ON CONFLICT DO NOTHING`. With 50 concurrent requests on one key, the first transaction inserts the row; the others block on the primary key until it commits, then see the conflict, roll back, and read the stored response. A stored hash that differs from the request's returns 422. Keys older than 24 hours are deleted hourly.

## Dispatch round

A worker takes one ride from the queue, then keeps reading until 2 s pass or it has 50 rides. For the batch it:

1. Takes `lock:ride:{id}` for each ride (`SET NX` with a 5 s TTL and a random token). Rides locked by another worker are skipped; that worker owns them.
2. Loads the rides and drops any that are no longer `REQUESTED` or `MATCHING`. This makes redelivered outbox events harmless.
3. Searches `vehicles:geo` within 3 km (6 km once if nothing is found), keeps the 10 nearest vehicles that are `AVAILABLE`, have a heartbeat newer than 30 s, and have not rejected this ride.
4. Builds a rides x vehicles matrix of A* ETAs from each candidate to the pickup; non-candidates are `+Inf`.
5. Solves it with Hungarian, or greedy if either side is above 200.
6. For each pair, takes `lock:vehicle:{id}` and runs `DispatchRide`. A refused commit increments `assignment_conflicts_total` and the ride is retried after 1 s. Rides with no vehicle are retried after 1 s too.

A sweeper runs every second: rides `DISPATCHED` for more than 15 s go back to `MATCHING` (reason `timeout`), and rides waiting more than 120 s become `FAILED`.

### Why three guards against double booking

- The Redis vehicle lock stops two workers from committing the same vehicle at the same moment. It is held only around the commit, so it does not stop two workers from choosing the same vehicle; the database refuses the second commit. It is not a correctness guarantee either: the TTL can expire during a slow commit, and Redis can lose the key.
- The ride update is `WHERE version = $v AND status = 'MATCHING'`, so a worker that read a ride before someone else changed it commits nothing. The vehicle update is `WHERE status = 'AVAILABLE'`.
- The partial unique index `uniq_active_ride_per_vehicle` makes two active rides for one vehicle impossible regardless of application code.

`TestDatabaseGuardsWithoutRedisLocks` calls `DispatchRide` from 20 goroutines on random pairs with no Redis locks at all and checks that exactly one ride per vehicle ends up active.

### Heartbeats and the cache

Every heartbeat updates the Redis vehicle hash. Postgres is written at most once per 5 s per vehicle, or immediately when the reported status differs from the last stored one. Ride-bound statuses (`DISPATCHED`, `EN_ROUTE`, `IN_TRIP`) are owned by the dispatcher; a heartbeat cannot overwrite them in Postgres.

The Redis upsert is a Lua script that applies a status only if the incoming row version is not older than the stored one. Without it, a heartbeat that read `AVAILABLE` from Postgres just before a dispatch could write to Redis just after the dispatcher removed the vehicle from `vehicles:geo`, and put it back. Location-only updates never touch status.

The heartbeat response includes `ride_id`, read from the vehicle hash. That is how a vehicle learns it has an offer.

## Outbox

The publisher selects up to 100 unpublished rows with `FOR UPDATE SKIP LOCKED`, hands each ride ID to the dispatch queue, then marks the rows published in the same transaction. Several publisher goroutines can run without delivering a row twice in normal operation (`TestConcurrentPublishersDeliverEachRowOnce`).

If the process dies after the hand-off and before the commit, the rows unlock unpublished and the next poll delivers them again. The queue drops a ride ID that is already queued, and the dispatcher skips rides that are no longer waiting, so a duplicate costs one extra lookup. On startup the dispatcher also enqueues up to 10,000 `REQUESTED` or `MATCHING` rides, because the in-process queue does not survive a restart.

## A* heuristic

`h(n) = haversine(n, goal) / max_speed`. Every edge connects two grid nodes and has length equal to the haversine distance between them, and no edge is faster than `max_speed`, so the true remaining time is at least `h(n)`: the heuristic is admissible. For an edge `(u, v)`, `h(u) <= cost(u, v) + h(v)` by the triangle inequality, so it is also consistent and each node is settled once. `TestAStarEqualsDijkstra` checks equal costs on 200 random pairs.

Locations are snapped to the nearest node in the largest connected component, so any two snapped locations are mutually reachable. Routes are cached in an LRU keyed by (from node, to node).

## Simulator

The simulator reuses `geo` and `assign` and needs no database. For each seed it generates demand (thinned non-homogeneous Poisson, 3 to 5 Gaussian hotspots, peak multiplier in the middle third, 1 to 5 km trips) and vehicle start nodes once, then runs each strategy on that scenario. Accept or reject outcomes are hashed from (seed, ride, attempt), so strategies see the same draw for the same attempt number.

Vehicles always travel the A* route; the strategy only changes the decision cost (straight-line ETA at 30 km/h or A* ETA) and the solver. Candidate search mirrors the service: straight-line radius 3 km then 6 km, top 10. A vehicle becomes available at its drop-off node when its trip time has elapsed.

## Metrics and alerts

All metrics are prefixed `dispatch_` and listed in `internal/metrics`. Alert rules in `deploy/prometheus/alerts.yml`:

- outbox backlog above 1000 rows for 1 minute
- assignment conflict rate above 0.5/s and above 3 times its 1-hour rate, for 2 minutes
- p95 time from request to accept above 30 s for 5 minutes
