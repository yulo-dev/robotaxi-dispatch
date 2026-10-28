// Package outbox publishes committed outbox rows to the dispatch queue.
//
// Delivery is at-least-once: a row is marked published only after its ride
// ID was handed to the sink, inside the same transaction that locked it. A
// crash between hand-off and commit re-delivers the row on the next poll, so
// the sink must be idempotent (the dispatcher deduplicates by ride ID and
// skips rides that are no longer waiting).
package outbox

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Sink receives ride IDs.
type Sink func(ctx context.Context, rideID string) error

// Observer receives publisher events.
type Observer interface {
	Published(n int)
	PublishError()
	Backlog(n int)
}

type nopObserver struct{}

func (nopObserver) Published(int) {}
func (nopObserver) PublishError() {}
func (nopObserver) Backlog(int)   {}

// Publisher polls outbox_events.
type Publisher struct {
	Pool      *pgxpool.Pool
	Sink      Sink
	BatchSize int
	Poll      time.Duration
	Workers   int
	Observer  Observer
	Log       *slog.Logger

	// beforeMark runs after the sink accepted a batch and before the rows
	// are marked published. Tests use it to simulate a crash.
	beforeMark func() error
}

// Run polls until ctx is done.
func (p *Publisher) Run(ctx context.Context) {
	p.defaults()
	var wg sync.WaitGroup
	for i := 0; i < p.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.loop(ctx)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		p.backlogLoop(ctx)
	}()
	wg.Wait()
}

func (p *Publisher) defaults() {
	if p.BatchSize <= 0 {
		p.BatchSize = 100
	}
	if p.Poll <= 0 {
		p.Poll = 200 * time.Millisecond
	}
	if p.Workers <= 0 {
		p.Workers = 1
	}
	if p.Observer == nil {
		p.Observer = nopObserver{}
	}
	if p.Log == nil {
		p.Log = slog.Default()
	}
}

func (p *Publisher) loop(ctx context.Context) {
	for ctx.Err() == nil {
		n, err := p.PublishOnce(ctx)
		if err != nil && ctx.Err() == nil {
			p.Observer.PublishError()
			p.Log.Warn("outbox publish failed", "err", err)
		}
		if n == p.BatchSize {
			continue // more rows are likely waiting
		}
		select {
		case <-ctx.Done():
		case <-time.After(p.Poll):
		}
	}
}

func (p *Publisher) backlogLoop(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		var n int
		if err := p.Pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`).Scan(&n); err == nil {
			p.Observer.Backlog(n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// PublishOnce publishes up to BatchSize unpublished rows and returns how many
// were marked published.
func (p *Publisher) PublishOnce(ctx context.Context) (int, error) {
	p.defaults()
	tx, err := p.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `SELECT id, aggregate_id FROM outbox_events
		WHERE published_at IS NULL ORDER BY id LIMIT $1 FOR UPDATE SKIP LOCKED`, p.BatchSize)
	if err != nil {
		return 0, err
	}
	type row struct {
		id     int64
		rideID string
	}
	batch, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		err := r.Scan(&x.id, &x.rideID)
		return x, err
	})
	if err != nil || len(batch) == 0 {
		return 0, err
	}
	var delivered, undelivered []int64
	var sinkErr error
	for _, r := range batch {
		if sinkErr == nil {
			sinkErr = p.Sink(ctx, r.rideID)
		}
		if sinkErr != nil {
			undelivered = append(undelivered, r.id)
			continue
		}
		delivered = append(delivered, r.id)
	}
	if p.beforeMark != nil {
		if err := p.beforeMark(); err != nil {
			return 0, err
		}
	}
	// Failed rows stay unpublished; their attempt count records the failure.
	if _, err := tx.Exec(ctx, `UPDATE outbox_events SET
			published_at = CASE WHEN id = ANY($1) THEN now() ELSE NULL END,
			attempts = attempts + 1
		WHERE id = ANY($1) OR id = ANY($2)`, delivered, undelivered); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	p.Observer.Published(len(delivered))
	return len(delivered), sinkErr
}
