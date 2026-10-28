//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/yulo-dev/robotaxi-dispatch/internal/store"
	"github.com/yulo-dev/robotaxi-dispatch/internal/testdb"
)

func TestMigrateIsIdempotent(t *testing.T) {
	st := testdb.Open(t)
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestPartialUniqueIndexBlocksSecondActiveRide(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	if _, err := st.CreateVehicle(ctx, store.Vehicle{ID: "v1", Status: store.VehicleAvailable, Lat: 37.7, Lng: -122.4, BatteryPct: 90}); err != nil {
		t.Fatal(err)
	}
	ins := `INSERT INTO rides (id, rider_id, pickup_lat, pickup_lng, dropoff_lat, dropoff_lng, status, vehicle_id, requested_at)
		VALUES (gen_random_uuid(), 'r', 0, 0, 0, 0, $1, 'v1', now())`
	if _, err := st.Pool().Exec(ctx, ins, "DISPATCHED"); err != nil {
		t.Fatal(err)
	}
	_, err := st.Pool().Exec(ctx, ins, "ASSIGNED")
	if err == nil {
		t.Fatal("second active ride for one vehicle was accepted")
	}
	// Finished rides do not count.
	if _, err := st.Pool().Exec(ctx, ins, "COMPLETED"); err != nil {
		t.Fatal(err)
	}
}

func TestHeartbeatDoesNotOverrideBusyStatus(t *testing.T) {
	st := testdb.Open(t)
	ctx := context.Background()
	if _, err := st.CreateVehicle(ctx, store.Vehicle{ID: "v1", Status: store.VehicleAvailable, Lat: 37.7, Lng: -122.4, BatteryPct: 90}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Pool().Exec(ctx, `UPDATE vehicles SET status='DISPATCHED' WHERE id='v1'`); err != nil {
		t.Fatal(err)
	}
	v, err := st.Heartbeat(ctx, "v1", 37.71, -122.41, 88, store.VehicleAvailable)
	if err != nil {
		t.Fatal(err)
	}
	if v.Status != store.VehicleDispatched || v.Lat != 37.71 {
		t.Fatalf("got %+v", v)
	}
	if _, err := st.Heartbeat(ctx, "missing", 0, 0, 0, ""); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing vehicle: %v", err)
	}
}
