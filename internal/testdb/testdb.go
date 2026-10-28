//go:build integration

// Package testdb provides a migrated, empty Postgres store for integration tests.
package testdb

import (
	"context"
	"os"
	"testing"

	"github.com/yulo-dev/robotaxi-dispatch/internal/cache"
	"github.com/yulo-dev/robotaxi-dispatch/internal/store"
)

// URL returns TEST_DATABASE_URL or the local default.
func URL() string {
	if u := os.Getenv("TEST_DATABASE_URL"); u != "" {
		return u
	}
	return "postgres://dispatch:dispatch@localhost:5432/dispatch?sslmode=disable"
}

// Open connects, migrates, and truncates every table.
func Open(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, URL(), 40)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := st.Pool().Exec(ctx, `TRUNCATE ride_rejections, rides, vehicles, outbox_events, idempotency_keys RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(st.Close)
	return st
}

// Redis returns a flushed Redis cache at TEST_REDIS_ADDR (default localhost:6379).
func Redis(t *testing.T) *cache.Cache {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	c := cache.New(addr, 0)
	if err := c.Client().FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("redis: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}
