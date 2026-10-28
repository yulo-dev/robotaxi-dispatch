// Package config loads service configuration from environment variables.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the API service configuration.
type Config struct {
	HTTPAddr        string
	DatabaseURL     string
	RedisAddr       string
	DBMaxConns      int32
	HeartbeatWrite  time.Duration // min interval between Postgres heartbeat writes per vehicle
	IdempotencyTTL  time.Duration
	BatchWindow     time.Duration
	BatchMax        int
	SearchRadiusM   float64
	ExpandRadiusM   float64
	TopK            int
	HeartbeatMaxAge time.Duration // candidates need a heartbeat newer than this
	DecisionTimeout time.Duration
	MatchTimeout    time.Duration
	MaxHungarian    int
	LockTTL         time.Duration
	DispatchWorkers int
	OutboxWorkers   int
	OutboxPoll      time.Duration
	QueueSize       int
	RouteCacheSize  int
}

// Load reads the environment, applying defaults.
func Load() (Config, error) {
	c := Config{
		HTTPAddr:        env("HTTP_ADDR", ":8080"),
		DatabaseURL:     env("DATABASE_URL", "postgres://dispatch:dispatch@localhost:5432/dispatch?sslmode=disable"),
		RedisAddr:       env("REDIS_ADDR", "localhost:6379"),
		DBMaxConns:      20,
		HeartbeatWrite:  5 * time.Second,
		IdempotencyTTL:  24 * time.Hour,
		BatchWindow:     2 * time.Second,
		BatchMax:        50,
		SearchRadiusM:   3000,
		ExpandRadiusM:   6000,
		TopK:            10,
		HeartbeatMaxAge: 30 * time.Second,
		DecisionTimeout: 15 * time.Second,
		MatchTimeout:    120 * time.Second,
		MaxHungarian:    200,
		LockTTL:         5 * time.Second,
		DispatchWorkers: 2,
		OutboxWorkers:   2,
		OutboxPoll:      200 * time.Millisecond,
		QueueSize:       10_000,
		RouteCacheSize:  100_000,
	}
	var err error
	setInt := func(name string, dst *int) {
		if v := os.Getenv(name); v != "" && err == nil {
			var n int
			n, err = strconv.Atoi(v)
			if err != nil {
				err = fmt.Errorf("%s: %w", name, err)
			}
			*dst = n
		}
	}
	setDur := func(name string, dst *time.Duration) {
		if v := os.Getenv(name); v != "" && err == nil {
			var d time.Duration
			d, err = time.ParseDuration(v)
			if err != nil {
				err = fmt.Errorf("%s: %w", name, err)
			}
			*dst = d
		}
	}
	maxConns := int(c.DBMaxConns)
	setInt("DB_MAX_CONNS", &maxConns)
	c.DBMaxConns = int32(maxConns)
	setDur("HEARTBEAT_WRITE_INTERVAL", &c.HeartbeatWrite)
	setDur("IDEMPOTENCY_TTL", &c.IdempotencyTTL)
	setDur("BATCH_WINDOW", &c.BatchWindow)
	setInt("BATCH_MAX", &c.BatchMax)
	setInt("TOP_K", &c.TopK)
	setDur("HEARTBEAT_MAX_AGE", &c.HeartbeatMaxAge)
	setDur("DECISION_TIMEOUT", &c.DecisionTimeout)
	setDur("MATCH_TIMEOUT", &c.MatchTimeout)
	setInt("MAX_HUNGARIAN", &c.MaxHungarian)
	setDur("LOCK_TTL", &c.LockTTL)
	setInt("DISPATCH_WORKERS", &c.DispatchWorkers)
	setInt("OUTBOX_WORKERS", &c.OutboxWorkers)
	setDur("OUTBOX_POLL", &c.OutboxPoll)
	setInt("DISPATCH_QUEUE_SIZE", &c.QueueSize)
	setInt("ROUTE_CACHE_SIZE", &c.RouteCacheSize)
	return c, err
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}
