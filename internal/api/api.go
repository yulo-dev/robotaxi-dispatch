// Package api implements the HTTP handlers, request validation, and the
// idempotency middleware.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/yulo-dev/robotaxi-dispatch/internal/store"
)

// VehicleCache mirrors vehicle location and status for fast candidate
// search. Implemented by the Redis cache; nil disables it.
type VehicleCache interface {
	UpsertVehicle(ctx context.Context, v store.Vehicle) error
	UpdateLocation(ctx context.Context, id string, lat, lng, battery float64, at time.Time) error
	Ping(ctx context.Context) error
}

// Dispatcher owns vehicle decisions and the trip lifecycle. Nil disables
// those endpoints.
type Dispatcher interface {
	Decide(ctx context.Context, rideID, vehicleID string, accept bool) (store.Ride, error)
	Pickup(ctx context.Context, rideID, vehicleID string) (store.Ride, error)
	Complete(ctx context.Context, rideID, vehicleID string) (store.Ride, error)
}

// Observer receives API events for metrics. All methods may be no-ops.
type Observer interface {
	RideRequested()
	IdempotentReplay()
	HTTPRequest(route string, code int, d time.Duration)
}

type nopObserver struct{}

func (nopObserver) RideRequested()                         {}
func (nopObserver) IdempotentReplay()                      {}
func (nopObserver) HTTPRequest(string, int, time.Duration) {}

// Server holds handler dependencies.
type Server struct {
	Store      *store.Store
	Cache      VehicleCache
	Dispatcher Dispatcher
	Observer   Observer
	Log        *slog.Logger

	// HeartbeatWriteInterval throttles Postgres heartbeat writes per vehicle.
	HeartbeatWriteInterval time.Duration
	// Extra routes (for example /metrics) mounted on the router.
	Extra map[string]http.Handler

	throttle sync.Map // vehicle id -> heartbeatState
	now      func() time.Time
}

type heartbeatState struct {
	at     time.Time
	status store.VehicleStatus
}

// Router builds the HTTP handler.
func (s *Server) Router() http.Handler {
	if s.Observer == nil {
		s.Observer = nopObserver{}
	}
	if s.Log == nil {
		s.Log = slog.Default()
	}
	if s.now == nil {
		s.now = time.Now
	}
	r := chi.NewRouter()
	r.Use(s.instrument)
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	r.Get("/readyz", s.readyz)
	for path, h := range s.Extra {
		r.Handle(path, h)
	}
	r.Route("/v1", func(r chi.Router) {
		r.Post("/vehicles", s.createVehicle)
		r.Get("/vehicles", s.listVehicles)
		r.Post("/vehicles/{id}/heartbeat", s.heartbeat)
		r.With(requireIdempotency).Post("/rides", s.createRide)
		r.Get("/rides/{id}", s.getRide)
		r.Post("/rides/{id}/cancel", s.cancelRide)
		r.Post("/rides/{id}/pickup", s.tripStep(func(d Dispatcher) func(context.Context, string, string) (store.Ride, error) { return d.Pickup }))
		r.Post("/rides/{id}/complete", s.tripStep(func(d Dispatcher) func(context.Context, string, string) (store.Ride, error) { return d.Complete }))
		r.Post("/dispatch/{ride_id}/decision", s.decision)
	})
	return r
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.code = code
	r.ResponseWriter.WriteHeader(code)
}

func (s *Server) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, code: http.StatusOK}
		next.ServeHTTP(rec, r)
		route := chi.RouteContext(r.Context()).RoutePattern()
		if route == "" {
			route = "unmatched"
		}
		s.Observer.HTTPRequest(route, rec.code, time.Since(start))
	})
}

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	checks := map[string]string{"postgres": "ok"}
	ok := true
	if err := s.Store.Ping(ctx); err != nil {
		checks["postgres"] = err.Error()
		ok = false
	}
	if s.Cache != nil {
		checks["redis"] = "ok"
		if err := s.Cache.Ping(ctx); err != nil {
			checks["redis"] = err.Error()
			ok = false
		}
	}
	code := http.StatusOK
	if !ok {
		code = http.StatusServiceUnavailable
	}
	writeJSON(w, code, checks)
}

// ---- helpers ----

type errorBody struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	var b errorBody
	b.Error.Code, b.Error.Message = code, msg
	writeJSON(w, status, b)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

func validLatLng(lat, lng float64) bool {
	return lat >= -90 && lat <= 90 && lng >= -180 && lng <= 180
}

func (s *Server) storeError(w http.ResponseWriter, err error) {
	var te *store.TransitionError
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", "resource not found")
	case errors.As(err, &te):
		writeError(w, http.StatusConflict, "illegal_transition", te.Error())
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrVehicleBusy), errors.Is(err, store.ErrWrongVehicle):
		writeError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, store.ErrExists):
		writeError(w, http.StatusConflict, "already_exists", err.Error())
	case errors.Is(err, store.ErrIdempotencyReuse):
		writeError(w, http.StatusUnprocessableEntity, "idempotency_key_reused", err.Error())
	default:
		s.Log.Error("request failed", "err", err)
		writeError(w, http.StatusInternalServerError, "internal", "internal error")
	}
}

// ---- vehicles ----

type vehicleRequest struct {
	ID         string              `json:"id"`
	Lat        float64             `json:"lat"`
	Lng        float64             `json:"lng"`
	BatteryPct float64             `json:"battery_pct"`
	Status     store.VehicleStatus `json:"status"`
}

func (s *Server) createVehicle(w http.ResponseWriter, r *http.Request) {
	var req vehicleRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if req.Status == "" {
		req.Status = store.VehicleAvailable
	}
	switch {
	case strings.TrimSpace(req.ID) == "" || len(req.ID) > 64:
		writeError(w, http.StatusBadRequest, "invalid_request", "id is required (max 64 characters)")
		return
	case !validLatLng(req.Lat, req.Lng):
		writeError(w, http.StatusBadRequest, "invalid_request", "lat/lng out of range")
		return
	case req.BatteryPct < 0 || req.BatteryPct > 100:
		writeError(w, http.StatusBadRequest, "invalid_request", "battery_pct must be 0 to 100")
		return
	case req.Status != store.VehicleAvailable && req.Status != store.VehicleOffline:
		writeError(w, http.StatusBadRequest, "invalid_request", "status must be AVAILABLE or OFFLINE")
		return
	}
	v, err := s.Store.CreateVehicle(r.Context(), store.Vehicle{
		ID: req.ID, Status: req.Status, Lat: req.Lat, Lng: req.Lng, BatteryPct: req.BatteryPct,
	})
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.syncCache(r.Context(), v)
	writeJSON(w, http.StatusCreated, v)
}

func (s *Server) syncCache(ctx context.Context, v store.Vehicle) {
	if s.Cache == nil {
		return
	}
	if err := s.Cache.UpsertVehicle(ctx, v); err != nil {
		s.Log.Warn("cache update failed", "vehicle", v.ID, "err", err)
	}
}

func (s *Server) listVehicles(w http.ResponseWriter, r *http.Request) {
	status := store.VehicleStatus(r.URL.Query().Get("status"))
	if status != "" && !store.ValidVehicleStatus(status) {
		writeError(w, http.StatusBadRequest, "invalid_request", "unknown status")
		return
	}
	vs, err := s.Store.ListVehicles(r.Context(), status, 1000)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"vehicles": vs})
}

type heartbeatRequest struct {
	Lat        float64             `json:"lat"`
	Lng        float64             `json:"lng"`
	BatteryPct float64             `json:"battery_pct"`
	Status     store.VehicleStatus `json:"status"`
}

// heartbeat always refreshes the cache; the Postgres row is written at most
// once per HeartbeatWriteInterval per vehicle unless the reported status changes.
func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var req heartbeatRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if !validLatLng(req.Lat, req.Lng) || req.BatteryPct < 0 || req.BatteryPct > 100 {
		writeError(w, http.StatusBadRequest, "invalid_request", "lat/lng or battery_pct out of range")
		return
	}
	if req.Status != "" && !store.ValidVehicleStatus(req.Status) {
		writeError(w, http.StatusBadRequest, "invalid_request", "unknown status")
		return
	}
	now := s.now()
	if prev, ok := s.throttle.Load(id); ok {
		st := prev.(heartbeatState)
		if now.Sub(st.at) < s.HeartbeatWriteInterval && (req.Status == "" || req.Status == st.status) {
			if s.Cache != nil {
				if err := s.Cache.UpdateLocation(r.Context(), id, req.Lat, req.Lng, req.BatteryPct, now); err != nil {
					s.Log.Warn("cache update failed", "vehicle", id, "err", err)
				}
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "persisted": false})
			return
		}
	}
	v, err := s.Store.Heartbeat(r.Context(), id, req.Lat, req.Lng, req.BatteryPct, req.Status)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.throttle.Store(id, heartbeatState{at: now, status: v.Status})
	s.syncCache(r.Context(), v)
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "status": v.Status, "persisted": true})
}

// ---- rides ----

type rideRequest struct {
	RiderID    string  `json:"rider_id"`
	PickupLat  float64 `json:"pickup_lat"`
	PickupLng  float64 `json:"pickup_lng"`
	DropoffLat float64 `json:"dropoff_lat"`
	DropoffLng float64 `json:"dropoff_lng"`
}

func (s *Server) createRide(w http.ResponseWriter, r *http.Request) {
	idem, _ := idempotencyFrom(r.Context())
	var req rideRequest
	if err := json.Unmarshal(idem.Body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	if strings.TrimSpace(req.RiderID) == "" || len(req.RiderID) > 64 {
		writeError(w, http.StatusBadRequest, "invalid_request", "rider_id is required (max 64 characters)")
		return
	}
	if !validLatLng(req.PickupLat, req.PickupLng) || !validLatLng(req.DropoffLat, req.DropoffLng) {
		writeError(w, http.StatusBadRequest, "invalid_request", "pickup/dropoff out of range")
		return
	}
	resp, replayed, err := s.Store.CreateRide(r.Context(), idem.Key, idem.Hash, store.NewRide{
		RiderID: req.RiderID, PickupLat: req.PickupLat, PickupLng: req.PickupLng,
		DropoffLat: req.DropoffLat, DropoffLng: req.DropoffLng,
	}, func(ride store.Ride) store.StoredResponse {
		b, _ := json.Marshal(ride)
		return store.StoredResponse{Status: http.StatusAccepted, Body: b}
	})
	if err != nil {
		s.storeError(w, err)
		return
	}
	if replayed {
		s.Observer.IdempotentReplay()
		w.Header().Set(headerReplayed, "true")
	} else {
		s.Observer.RideRequested()
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.Status)
	_, _ = w.Write(resp.Body)
}

func (s *Server) getRide(w http.ResponseWriter, r *http.Request) {
	ride, err := s.Store.GetRide(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ride)
}

func (s *Server) cancelRide(w http.ResponseWriter, r *http.Request) {
	ride, freed, err := s.Store.CancelRide(r.Context(), chi.URLParam(r, "id"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	if freed != "" {
		if v, err := s.Store.GetVehicle(r.Context(), freed); err == nil {
			s.syncCache(r.Context(), v)
		}
	}
	writeJSON(w, http.StatusOK, ride)
}

type decisionRequest struct {
	VehicleID string `json:"vehicle_id"`
	Decision  string `json:"decision"`
}

func (s *Server) decision(w http.ResponseWriter, r *http.Request) {
	if s.Dispatcher == nil {
		writeError(w, http.StatusServiceUnavailable, "dispatcher_unavailable", "dispatcher not running")
		return
	}
	var req decisionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	var accept bool
	switch strings.ToUpper(req.Decision) {
	case "ACCEPT":
		accept = true
	case "REJECT":
	default:
		writeError(w, http.StatusBadRequest, "invalid_request", "decision must be ACCEPT or REJECT")
		return
	}
	if req.VehicleID == "" {
		writeError(w, http.StatusBadRequest, "invalid_request", "vehicle_id is required")
		return
	}
	ride, err := s.Dispatcher.Decide(r.Context(), chi.URLParam(r, "ride_id"), req.VehicleID, accept)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, ride)
}

type vehicleRef struct {
	VehicleID string `json:"vehicle_id"`
}

// tripStep handles pickup and complete: body {vehicle_id}; 409 when the
// transition is illegal or the vehicle is not the ride's vehicle.
func (s *Server) tripStep(pick func(Dispatcher) func(context.Context, string, string) (store.Ride, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Dispatcher == nil {
			writeError(w, http.StatusServiceUnavailable, "dispatcher_unavailable", "dispatcher not running")
			return
		}
		var req vehicleRef
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid_json", err.Error())
			return
		}
		if req.VehicleID == "" {
			writeError(w, http.StatusBadRequest, "invalid_request", "vehicle_id is required")
			return
		}
		ride, err := pick(s.Dispatcher)(r.Context(), chi.URLParam(r, "id"), req.VehicleID)
		if err != nil {
			s.storeError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, ride)
	}
}
