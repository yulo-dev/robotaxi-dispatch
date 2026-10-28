// k6 load test: 200 vehicles heartbeating every 2 s and ride requests ramping
// from 10 to 100 per second over 3 minutes. 5% of ride requests resend a
// previous Idempotency-Key with the same body.
//
// Each simulated vehicle learns about a dispatch from its heartbeat
// response (ride_id), accepts (90%) or rejects it, then calls pickup and
// complete on later heartbeats so the fleet recycles during the run.
//
//   k6 run -e BASE_URL=http://localhost:8080 scripts/loadtest/dispatch.js
import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter } from 'k6/metrics';
import exec from 'k6/execution';

const BASE = __ENV.BASE_URL || 'http://localhost:8080';
const VEHICLES = parseInt(__ENV.VEHICLES || '200', 10);
const ACCEPT_PROB = parseFloat(__ENV.ACCEPT_PROB || '0.9');
const REPLAY_PROB = 0.05;

// San Francisco box shared with the service's road network.
const BOX = { minLat: 37.70, maxLat: 37.81, minLng: -122.52, maxLng: -122.36 };
const JSON_HEADERS = { 'Content-Type': 'application/json' };

const uniqueRides = new Counter('ride_requests_unique');
const replayedRides = new Counter('ride_requests_replayed');
const replayMismatch = new Counter('ride_replay_mismatch');
const decisions = new Counter('vehicle_decisions');
const tripsCompleted = new Counter('trips_completed');

http.setResponseCallback(http.expectedStatuses(200, 201, 202));

export const options = {
  scenarios: {
    vehicles: {
      executor: 'constant-vus',
      exec: 'vehicle',
      vus: VEHICLES,
      duration: '3m30s',
    },
    riders: {
      executor: 'ramping-arrival-rate',
      exec: 'rider',
      startRate: 10,
      timeUnit: '1s',
      preAllocatedVUs: 100,
      maxVUs: 400,
      startTime: '10s', // let every vehicle send a first heartbeat
      stages: [{ target: 100, duration: '3m' }],
    },
  },
  // Always-true thresholds so k6 reports per-endpoint latency.
  thresholds: {
    'http_req_duration{name:POST /v1/rides}': ['max>=0'],
    'http_req_duration{name:POST /v1/vehicles/{id}/heartbeat}': ['max>=0'],
    'http_req_duration{name:POST /v1/dispatch/{ride_id}/decision}': ['max>=0'],
    'http_req_duration{name:POST /v1/rides/{id}/pickup}': ['max>=0'],
    'http_req_duration{name:POST /v1/rides/{id}/complete}': ['max>=0'],
    'http_req_failed': ['rate>=0'],
  },
  summaryTrendStats: ['avg', 'p(50)', 'p(95)', 'p(99)', 'max'],
};

function rand(min, max) { return min + Math.random() * (max - min); }
function point() { return { lat: rand(BOX.minLat, BOX.maxLat), lng: rand(BOX.minLng, BOX.maxLng) }; }

// One run ID shared by every VU so keys and vehicle IDs never collide with
// an earlier run against the same database.
export function setup() {
  replayMismatch.add(0); // report the counter even when nothing mismatches
  return { runId: __ENV.RUN_ID || `${Date.now()}` };
}

// Per-VU state (each VU keeps its own copy across iterations).
let vehicleID = null;
let pos = null;
let trip = null; // { id, stage: 'accepted' | 'picked_up' }

export function vehicle(data) {
  if (vehicleID === null) {
    // k6 numbers VUs across all scenarios, so each vehicle VU registers itself.
    vehicleID = `lt-${data.runId}-v${exec.vu.idInTest}`;
    pos = point();
    const res = http.post(`${BASE}/v1/vehicles`, JSON.stringify({ id: vehicleID, lat: pos.lat, lng: pos.lng, battery_pct: 90 }),
      { headers: JSON_HEADERS, tags: { name: 'POST /v1/vehicles' } });
    check(res, { 'vehicle registered': (r) => r.status === 201 });
  }
  const id = vehicleID;
  const hb = http.post(`${BASE}/v1/vehicles/${id}/heartbeat`,
    JSON.stringify({ lat: pos.lat, lng: pos.lng, battery_pct: 80 }),
    { headers: JSON_HEADERS, tags: { name: 'POST /v1/vehicles/{id}/heartbeat' } });
  check(hb, { 'heartbeat 200': (r) => r.status === 200 });
  const rideID = hb.status === 200 ? hb.json('ride_id') : null;

  if (rideID && (!trip || trip.id !== rideID)) {
    // New offer.
    const accept = Math.random() < ACCEPT_PROB;
    const res = http.post(`${BASE}/v1/dispatch/${rideID}/decision`,
      JSON.stringify({ vehicle_id: id, decision: accept ? 'ACCEPT' : 'REJECT' }),
      { headers: JSON_HEADERS, tags: { name: 'POST /v1/dispatch/{ride_id}/decision' } });
    decisions.add(1, { decision: accept ? 'accept' : 'reject' });
    trip = accept && res.status === 200 ? { id: rideID, stage: 'accepted' } : null;
  } else if (trip && trip.stage === 'accepted') {
    const res = http.post(`${BASE}/v1/rides/${trip.id}/pickup`, JSON.stringify({ vehicle_id: id }),
      { headers: JSON_HEADERS, tags: { name: 'POST /v1/rides/{id}/pickup' } });
    trip = res.status === 200 ? { id: trip.id, stage: 'picked_up' } : null;
  } else if (trip && trip.stage === 'picked_up') {
    const res = http.post(`${BASE}/v1/rides/${trip.id}/complete`, JSON.stringify({ vehicle_id: id }),
      { headers: JSON_HEADERS, tags: { name: 'POST /v1/rides/{id}/complete' } });
    if (res.status === 200) tripsCompleted.add(1);
    pos = point(); // drop-off somewhere else in the city
    trip = null;
  } else if (!rideID) {
    trip = null;
  }
  // Heartbeat every 2 s.
  const elapsed = (Date.now() - exec.scenario.startTime) / 1000;
  const next = Math.ceil((elapsed + 0.001) / 2) * 2;
  const wait = next - elapsed;
  if (wait > 0) sleep(wait);
}

let last = null; // { key, body } for idempotent resends

export function rider(data) {
  let key; let body; let replay = false;
  if (last !== null && Math.random() < REPLAY_PROB) {
    ({ key, body } = last);
    replay = true;
  } else {
    const p = point();
    const dLat = rand(-0.03, 0.03); const dLng = rand(-0.03, 0.03);
    key = `lt-${data.runId}-${exec.vu.idInTest}-${exec.vu.iterationInScenario}`;
    body = JSON.stringify({
      rider_id: `rider-${exec.vu.idInTest}`,
      pickup_lat: p.lat, pickup_lng: p.lng,
      dropoff_lat: Math.min(BOX.maxLat, Math.max(BOX.minLat, p.lat + dLat)),
      dropoff_lng: Math.min(BOX.maxLng, Math.max(BOX.minLng, p.lng + dLng)),
    });
  }
  const res = http.post(`${BASE}/v1/rides`, body,
    { headers: Object.assign({ 'Idempotency-Key': key }, JSON_HEADERS), tags: { name: 'POST /v1/rides' } });
  check(res, { 'ride accepted': (r) => r.status === 202 });
  if (replay) {
    replayedRides.add(1);
    if (res.headers['Idempotent-Replayed'] !== 'true' || (last.id && res.json('id') !== last.id)) replayMismatch.add(1);
  } else if (res.status === 202) {
    uniqueRides.add(1);
    last = { key, body, id: res.json('id') };
  }
}
