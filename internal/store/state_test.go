package store

import "testing"

func TestRideStateMachine(t *testing.T) {
	legal := [][2]RideStatus{
		{RideRequested, RideMatching},
		{RideMatching, RideDispatched},
		{RideDispatched, RideAssigned},
		{RideAssigned, RideInTrip},
		{RideInTrip, RideCompleted},
		{RideDispatched, RideMatching},
		{RideMatching, RideFailed},
		{RideRequested, RideFailed},
		{RideRequested, RideCancelled},
		{RideMatching, RideCancelled},
		{RideDispatched, RideCancelled},
		{RideAssigned, RideCancelled},
	}
	for _, p := range legal {
		if !CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be legal", p[0], p[1])
		}
	}
	illegal := [][2]RideStatus{
		{RideRequested, RideDispatched},
		{RideRequested, RideCompleted},
		{RideMatching, RideAssigned},
		{RideAssigned, RideMatching},
		{RideInTrip, RideCancelled},
		{RideCompleted, RideCancelled},
		{RideCancelled, RideMatching},
		{RideFailed, RideMatching},
		{RideDispatched, RideFailed},
	}
	for _, p := range illegal {
		if CanTransition(p[0], p[1]) {
			t.Errorf("%s -> %s should be illegal", p[0], p[1])
		}
	}
	for _, s := range []RideStatus{RideCompleted, RideFailed, RideCancelled} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	if RideMatching.Terminal() {
		t.Error("MATCHING is not terminal")
	}
}

func TestVehicleStatus(t *testing.T) {
	for _, s := range []VehicleStatus{VehicleDispatched, VehicleEnRoute, VehicleInTrip} {
		if !s.Busy() {
			t.Errorf("%s should be busy", s)
		}
	}
	if VehicleAvailable.Busy() || VehicleOffline.Busy() {
		t.Error("available/offline are not busy")
	}
	if ValidVehicleStatus("PARKED") {
		t.Error("unknown status accepted")
	}
}
