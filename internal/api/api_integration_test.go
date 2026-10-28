//go:build integration

package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yulo-dev/robotaxi-dispatch/internal/api"
	"github.com/yulo-dev/robotaxi-dispatch/internal/store"
	"github.com/yulo-dev/robotaxi-dispatch/internal/testdb"
)

func newServer(t *testing.T) (*httptest.Server, *store.Store) {
	t.Helper()
	st := testdb.Open(t)
	srv := &api.Server{Store: st, HeartbeatWriteInterval: 5 * time.Second}
	ts := httptest.NewServer(srv.Router())
	t.Cleanup(ts.Close)
	return ts, st
}

type result struct {
	StatusCode int
	Header     http.Header
}

func do(t *testing.T, method, url, key string, body any) (result, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if s, ok := body.(string); ok {
			buf.WriteString(s)
		} else if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	_ = resp.Body.Close()
	return result{StatusCode: resp.StatusCode, Header: resp.Header}, out.Bytes()
}

const rideBody = `{"rider_id":"rider-1","pickup_lat":37.77,"pickup_lng":-122.42,"dropoff_lat":37.79,"dropoff_lng":-122.40}`

func count(t *testing.T, st *store.Store, sql string) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestConcurrentSameIdempotencyKeyCreatesOneRide(t *testing.T) {
	ts, st := newServer(t)
	const n = 50
	ids := make([]string, n)
	codes := make([]int, n)
	replays := make([]bool, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			resp, body := do(t, http.MethodPost, ts.URL+"/v1/rides", "same-key", rideBody)
			codes[i] = resp.StatusCode
			replays[i] = resp.Header.Get("Idempotent-Replayed") == "true"
			var r store.Ride
			_ = json.Unmarshal(body, &r)
			ids[i] = r.ID
		}(i)
	}
	close(start)
	wg.Wait()
	nReplay := 0
	for i := 0; i < n; i++ {
		if codes[i] != http.StatusAccepted {
			t.Fatalf("request %d: status %d", i, codes[i])
		}
		if ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("request %d returned ride %q, want %q", i, ids[i], ids[0])
		}
		if replays[i] {
			nReplay++
		}
	}
	if nReplay != n-1 {
		t.Fatalf("%d replays, want %d", nReplay, n-1)
	}
	if got := count(t, st, `SELECT count(*) FROM rides`); got != 1 {
		t.Fatalf("%d rides in database, want 1", got)
	}
	if got := count(t, st, `SELECT count(*) FROM outbox_events WHERE event_type='ride.requested'`); got != 1 {
		t.Fatalf("%d outbox events, want 1", got)
	}
}

func TestIdempotencyKeyReuseWithDifferentBody(t *testing.T) {
	ts, st := newServer(t)
	if resp, _ := do(t, http.MethodPost, ts.URL+"/v1/rides", "k1", rideBody); resp.StatusCode != http.StatusAccepted {
		t.Fatalf("first: %d", resp.StatusCode)
	}
	// Same content, different formatting: replay.
	reordered := `{"pickup_lng":-122.42, "rider_id":"rider-1","pickup_lat":37.77,"dropoff_lat":37.79,"dropoff_lng":-122.40}`
	if resp, _ := do(t, http.MethodPost, ts.URL+"/v1/rides", "k1", reordered); resp.StatusCode != http.StatusAccepted ||
		resp.Header.Get("Idempotent-Replayed") != "true" {
		t.Fatalf("reordered body should replay, got %d", resp.StatusCode)
	}
	other := `{"rider_id":"rider-2","pickup_lat":37.77,"pickup_lng":-122.42,"dropoff_lat":37.79,"dropoff_lng":-122.40}`
	resp, body := do(t, http.MethodPost, ts.URL+"/v1/rides", "k1", other)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("different body: status %d, want 422 (%s)", resp.StatusCode, body)
	}
	if got := count(t, st, `SELECT count(*) FROM rides`); got != 1 {
		t.Fatalf("%d rides, want 1", got)
	}
}

func TestCreateRideValidation(t *testing.T) {
	ts, _ := newServer(t)
	if resp, _ := do(t, http.MethodPost, ts.URL+"/v1/rides", "", rideBody); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing key: %d", resp.StatusCode)
	}
	bad := `{"rider_id":"","pickup_lat":37.77,"pickup_lng":-122.42,"dropoff_lat":37.79,"dropoff_lng":-122.40}`
	resp, body := do(t, http.MethodPost, ts.URL+"/v1/rides", "k-bad", bad)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty rider: %d", resp.StatusCode)
	}
	var e struct {
		Error struct{ Code, Message string } `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Code == "" {
		t.Fatalf("error body %s", body)
	}
}

func TestRideGetAndCancel(t *testing.T) {
	ts, _ := newServer(t)
	_, body := do(t, http.MethodPost, ts.URL+"/v1/rides", "k-cancel", rideBody)
	var r store.Ride
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	resp, body := do(t, http.MethodGet, ts.URL+"/v1/rides/"+r.ID, "", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get: %d", resp.StatusCode)
	}
	_ = json.Unmarshal(body, &r)
	if r.Status != store.RideRequested {
		t.Fatalf("status %s", r.Status)
	}
	if resp, _ := do(t, http.MethodPost, ts.URL+"/v1/rides/"+r.ID+"/cancel", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %d", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodPost, ts.URL+"/v1/rides/"+r.ID+"/cancel", "", nil); resp.StatusCode != http.StatusConflict {
		t.Fatalf("second cancel: %d, want 409", resp.StatusCode)
	}
	if resp, _ := do(t, http.MethodGet, ts.URL+"/v1/rides/not-a-uuid", "", nil); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bad id: %d", resp.StatusCode)
	}
}

func TestVehicleRegisterListHeartbeat(t *testing.T) {
	ts, st := newServer(t)
	for i := 0; i < 3; i++ {
		v := map[string]any{"id": fmt.Sprintf("v%d", i), "lat": 37.77, "lng": -122.42, "battery_pct": 80}
		if resp, body := do(t, http.MethodPost, ts.URL+"/v1/vehicles", "", v); resp.StatusCode != http.StatusCreated {
			t.Fatalf("register: %d %s", resp.StatusCode, body)
		}
	}
	if resp, _ := do(t, http.MethodPost, ts.URL+"/v1/vehicles", "", map[string]any{"id": "v0", "lat": 37.7, "lng": -122.4, "battery_pct": 50}); resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate: %d", resp.StatusCode)
	}
	hb := func(id, status string) map[string]any {
		_, body := do(t, http.MethodPost, ts.URL+"/v1/vehicles/"+id+"/heartbeat", "",
			map[string]any{"lat": 37.78, "lng": -122.41, "battery_pct": 70, "status": status})
		var out map[string]any
		_ = json.Unmarshal(body, &out)
		return out
	}
	if out := hb("v0", ""); out["persisted"] != true {
		t.Fatalf("first heartbeat not persisted: %v", out)
	}
	if out := hb("v0", ""); out["persisted"] != false {
		t.Fatalf("second heartbeat within 5 s persisted: %v", out)
	}
	if out := hb("v0", "OFFLINE"); out["persisted"] != true || out["status"] != "OFFLINE" {
		t.Fatalf("status change must bypass throttle: %v", out)
	}
	if resp, _ := do(t, http.MethodPost, ts.URL+"/v1/vehicles/nope/heartbeat", "",
		map[string]any{"lat": 37.78, "lng": -122.41, "battery_pct": 70}); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown vehicle: %d", resp.StatusCode)
	}
	_, body := do(t, http.MethodGet, ts.URL+"/v1/vehicles?status=AVAILABLE", "", nil)
	var list struct{ Vehicles []store.Vehicle }
	_ = json.Unmarshal(body, &list)
	if len(list.Vehicles) != 2 {
		t.Fatalf("available vehicles %d, want 2", len(list.Vehicles))
	}
	if got := count(t, st, `SELECT count(*) FROM vehicles WHERE status='OFFLINE'`); got != 1 {
		t.Fatalf("offline in db %d", got)
	}
}

func TestReadyz(t *testing.T) {
	ts, _ := newServer(t)
	if resp, _ := do(t, http.MethodGet, ts.URL+"/readyz", "", nil); resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz %d", resp.StatusCode)
	}
}
