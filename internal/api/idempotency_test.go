package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestHashCanonical(t *testing.T) {
	a, err := RequestHash([]byte(`{"rider_id":"r1","pickup_lat":37.7,"pickup_lng":-122.4}`))
	if err != nil {
		t.Fatal(err)
	}
	b, err := RequestHash([]byte("{\n  \"pickup_lng\": -122.4,\n  \"pickup_lat\": 37.7, \"rider_id\": \"r1\"\n}"))
	if err != nil {
		t.Fatal(err)
	}
	if a != b {
		t.Fatal("key order or whitespace changed the hash")
	}
	c, _ := RequestHash([]byte(`{"rider_id":"r2","pickup_lat":37.7,"pickup_lng":-122.4}`))
	if a == c {
		t.Fatal("different bodies hashed equal")
	}
	d, _ := RequestHash([]byte(`{"rider_id":"r1","pickup_lat":37.70,"pickup_lng":-122.4}`))
	if a == d {
		// Numbers keep their literal form; a retry must resend the same values.
		t.Fatal("numeric literal 37.70 hashed equal to 37.7")
	}
	if _, err := RequestHash([]byte(`{"a":1} {"b":2}`)); err == nil {
		t.Fatal("trailing JSON accepted")
	}
	if _, err := RequestHash([]byte(`not json`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestRequireIdempotencyHeader(t *testing.T) {
	called := false
	h := requireIdempotency(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if v, ok := idempotencyFrom(r.Context()); !ok || v.Key != "k1" || v.Hash == "" {
			t.Errorf("context missing idempotency data: %+v", v)
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/rides", strings.NewReader(`{}`)))
	if rec.Code != http.StatusBadRequest || called {
		t.Fatalf("missing key: code %d called %v", rec.Code, called)
	}
	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/rides", strings.NewReader(`{"x":1}`))
	req.Header.Set(headerIdempotencyKey, "k1")
	h.ServeHTTP(rec, req)
	if !called {
		t.Fatal("handler not called with key")
	}
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/v1/rides", strings.NewReader(`{bad`))
	req.Header.Set(headerIdempotencyKey, "k2")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("bad json: code %d", rec.Code)
	}
}
