// Package store is the Postgres repository for vehicles, rides, the
// transactional outbox, and idempotency keys.
package store

import (
	"errors"
	"fmt"
	"time"
)

// RideStatus is a ride state.
type RideStatus string

const (
	RideRequested  RideStatus = "REQUESTED"
	RideMatching   RideStatus = "MATCHING"
	RideDispatched RideStatus = "DISPATCHED"
	RideAssigned   RideStatus = "ASSIGNED"
	RideInTrip     RideStatus = "IN_TRIP"
	RideCompleted  RideStatus = "COMPLETED"
	RideFailed     RideStatus = "FAILED"
	RideCancelled  RideStatus = "CANCELLED"
)

// VehicleStatus is a vehicle state.
type VehicleStatus string

const (
	VehicleAvailable  VehicleStatus = "AVAILABLE"
	VehicleDispatched VehicleStatus = "DISPATCHED"
	VehicleEnRoute    VehicleStatus = "EN_ROUTE"
	VehicleInTrip     VehicleStatus = "IN_TRIP"
	VehicleOffline    VehicleStatus = "OFFLINE"
)

// ValidVehicleStatus reports whether s is a known vehicle status.
func ValidVehicleStatus(s VehicleStatus) bool {
	switch s {
	case VehicleAvailable, VehicleDispatched, VehicleEnRoute, VehicleInTrip, VehicleOffline:
		return true
	}
	return false
}

// Busy reports whether the vehicle is bound to a ride.
func (s VehicleStatus) Busy() bool {
	return s == VehicleDispatched || s == VehicleEnRoute || s == VehicleInTrip
}

// rideTransitions is the ride state machine:
//
//	REQUESTED -> MATCHING -> DISPATCHED -> ASSIGNED -> IN_TRIP -> COMPLETED
//	MATCHING -> FAILED            (no vehicle within the timeout)
//	DISPATCHED -> MATCHING        (vehicle rejects or does not answer)
//	REQUESTED/MATCHING/DISPATCHED/ASSIGNED -> CANCELLED
//
// REQUESTED -> FAILED is also allowed so a ride that never reached a
// dispatcher still expires.
var rideTransitions = map[RideStatus][]RideStatus{
	RideRequested:  {RideMatching, RideCancelled, RideFailed},
	RideMatching:   {RideDispatched, RideFailed, RideCancelled},
	RideDispatched: {RideAssigned, RideMatching, RideCancelled},
	RideAssigned:   {RideInTrip, RideCancelled},
	RideInTrip:     {RideCompleted},
}

// CanTransition reports whether from -> to is a legal ride transition.
func CanTransition(from, to RideStatus) bool {
	for _, s := range rideTransitions[from] {
		if s == to {
			return true
		}
	}
	return false
}

// Terminal reports whether no further transitions are possible.
func (s RideStatus) Terminal() bool { return len(rideTransitions[s]) == 0 }

// Vehicle is a fleet vehicle.
type Vehicle struct {
	ID              string        `json:"id"`
	Status          VehicleStatus `json:"status"`
	Lat             float64       `json:"lat"`
	Lng             float64       `json:"lng"`
	BatteryPct      float64       `json:"battery_pct"`
	LastHeartbeatAt time.Time     `json:"last_heartbeat_at"`
	Version         int64         `json:"version"`
}

// Ride is a ride request.
type Ride struct {
	ID          string     `json:"id"`
	RiderID     string     `json:"rider_id"`
	PickupLat   float64    `json:"pickup_lat"`
	PickupLng   float64    `json:"pickup_lng"`
	DropoffLat  float64    `json:"dropoff_lat"`
	DropoffLng  float64    `json:"dropoff_lng"`
	Status      RideStatus `json:"status"`
	VehicleID   *string    `json:"vehicle_id"`
	RequestedAt time.Time  `json:"requested_at"`
	AssignedAt  *time.Time `json:"assigned_at"`
	PickedUpAt  *time.Time `json:"picked_up_at"`
	CompletedAt *time.Time `json:"completed_at"`
	// DispatchedAt is when the current vehicle was offered the ride.
	DispatchedAt *time.Time `json:"dispatched_at"`
	Version      int64      `json:"version"`
}

// Errors returned by the repository.
var (
	ErrNotFound         = errors.New("not found")
	ErrConflict         = errors.New("version conflict")
	ErrVehicleBusy      = errors.New("vehicle already has an active ride")
	ErrIdempotencyReuse = errors.New("idempotency key reused with a different request")
	ErrExists           = errors.New("already exists")
)

// TransitionError is returned for an illegal state change.
type TransitionError struct {
	From, To RideStatus
}

func (e *TransitionError) Error() string {
	return fmt.Sprintf("illegal ride transition %s -> %s", e.From, e.To)
}
