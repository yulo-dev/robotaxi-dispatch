//go:build integration

package outbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/yulo-dev/robotaxi-dispatch/internal/dispatch"
	"github.com/yulo-dev/robotaxi-dispatch/internal/geo"
	"github.com/yulo-dev/robotaxi-dispatch/internal/store"
	"github.com/yulo-dev/robotaxi-dispatch/internal/testdb"
)

func createRide(t *testing.T, st *store.Store, key string) string {
	t.Helper()
	resp, _, err := st.CreateRide(context.Background(), key, key, store.NewRide{RiderID: "r", PickupLat: 37.77, PickupLng: -122.42, DropoffLat: 37.78, DropoffLng: -122.41},
		func(r store.Ride) store.StoredResponse {
			b, _ := json.Marshal(r)
			return store.StoredResponse{Status: 202, Body: b}
		})
	if err != nil {
		t.Fatal(err)
	}
	var r store.Ride
	_ = json.Unmarshal(resp.Body, &r)
	return r.ID
}

func unpublished(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func unpublishedCount(st *store.Store) int {
	var n int
	_ = st.Pool().QueryRow(context.Background(), `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&n)
	return n
}

type recorder struct {
	mu  sync.Mutex
	ids []string
}

func (r *recorder) sink(_ context.Context, id string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return nil
}

func TestEveryCommittedRideProducesAnEvent(t *testing.T) {
	st := testdb.Open(t)
	id := createRide(t, st, "k1")
	// The publisher was not running when the ride committed (killed before
	// publish). The row is durable; a fresh publisher delivers it.
	if unpublished(t, st) != 1 {
		t.Fatal("outbox row missing after commit")
	}
	rec := &recorder{}
	p := &Publisher{Pool: st.Pool(), Sink: rec.sink}
	if n, err := p.PublishOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("publish: n=%d err=%v", n, err)
	}
	if len(rec.ids) != 1 || rec.ids[0] != id {
		t.Fatalf("sink got %v", rec.ids)
	}
	if n, _ := p.PublishOnce(context.Background()); n != 0 || len(rec.ids) != 1 {
		t.Fatal("published row delivered again")
	}
}

func TestCrashBetweenHandoffAndMarkIsDeliveredOnceAfterDedupe(t *testing.T) {
	st := testdb.Open(t)
	c := testdb.Redis(t)
	n, err := geo.NewGrid(geo.DefaultGridConfig())
	if err != nil {
		t.Fatal(err)
	}
	queue := dispatch.New(dispatch.DefaultConfig(), st, c, geo.NewRouter(n, 0), nil, nil) // workers not started
	id := createRide(t, st, "k1")

	var delivered []string
	sink := func(ctx context.Context, rideID string) error {
		delivered = append(delivered, rideID)
		return queue.Enqueue(ctx, rideID)
	}
	crashed := &Publisher{Pool: st.Pool(), Sink: sink, beforeMark: func() error { return errors.New("process killed") }}
	if _, err := crashed.PublishOnce(context.Background()); err == nil {
		t.Fatal("expected simulated crash")
	}
	if unpublished(t, st) != 1 {
		t.Fatal("row must stay unpublished after a crash before commit")
	}
	restarted := &Publisher{Pool: st.Pool(), Sink: sink}
	if n, err := restarted.PublishOnce(context.Background()); err != nil || n != 1 {
		t.Fatalf("restart publish: n=%d err=%v", n, err)
	}
	if len(delivered) != 2 {
		t.Fatalf("at-least-once: delivered %d times, want 2", len(delivered))
	}
	if queue.QueueDepth() != 1 {
		t.Fatalf("queue holds %d entries for ride %s, want 1", queue.QueueDepth(), id)
	}
	if unpublished(t, st) != 0 {
		t.Fatal("row not marked published after restart")
	}
}

func TestConcurrentPublishersDeliverEachRowOnce(t *testing.T) {
	st := testdb.Open(t)
	want := map[string]bool{}
	for i := 0; i < 300; i++ {
		want[createRide(t, st, fmt.Sprintf("k%d", i))] = true
	}
	rec := &recorder{}
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p := &Publisher{Pool: st.Pool(), Sink: rec.sink, BatchSize: 25}
			for {
				n, err := p.PublishOnce(context.Background())
				if err != nil {
					t.Error(err)
					return
				}
				if n == 0 && unpublishedCount(st) == 0 {
					return // n == 0 alone can mean other workers hold the rows
				}
			}
		}()
	}
	wg.Wait()
	seen := map[string]int{}
	for _, id := range rec.ids {
		seen[id]++
	}
	if len(rec.ids) != 300 || len(seen) != 300 {
		t.Fatalf("delivered %d rows (%d unique), want 300", len(rec.ids), len(seen))
	}
	for id := range want {
		if seen[id] != 1 {
			t.Fatalf("ride %s delivered %d times", id, seen[id])
		}
	}
	if unpublished(t, st) != 0 {
		t.Fatal("rows left unpublished")
	}
}

func TestSinkFailureLeavesRowUnpublished(t *testing.T) {
	st := testdb.Open(t)
	createRide(t, st, "k1")
	p := &Publisher{Pool: st.Pool(), Sink: func(context.Context, string) error { return errors.New("queue closed") }}
	if _, err := p.PublishOnce(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	var attempts int
	if err := st.Pool().QueryRow(context.Background(), `SELECT attempts FROM outbox_events`).Scan(&attempts); err != nil {
		t.Fatal(err)
	}
	if unpublished(t, st) != 1 || attempts != 1 {
		t.Fatalf("unpublished=%d attempts=%d", unpublished(t, st), attempts)
	}
}
