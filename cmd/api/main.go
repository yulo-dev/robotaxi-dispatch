// Command api runs the dispatch HTTP service.
//
//	api           serve HTTP, the outbox publisher, and the dispatcher
//	api migrate   apply SQL migrations and exit
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/yulo-dev/robotaxi-dispatch/internal/api"
	"github.com/yulo-dev/robotaxi-dispatch/internal/cache"
	"github.com/yulo-dev/robotaxi-dispatch/internal/config"
	"github.com/yulo-dev/robotaxi-dispatch/internal/dispatch"
	"github.com/yulo-dev/robotaxi-dispatch/internal/geo"
	"github.com/yulo-dev/robotaxi-dispatch/internal/metrics"
	"github.com/yulo-dev/robotaxi-dispatch/internal/outbox"
	"github.com/yulo-dev/robotaxi-dispatch/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(log)
	if err := run(log); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	st, err := store.Open(ctx, cfg.DatabaseURL, cfg.DBMaxConns)
	if err != nil {
		return err
	}
	defer st.Close()

	if len(os.Args) > 1 && os.Args[1] == "migrate" {
		if err := st.Migrate(ctx); err != nil {
			return err
		}
		log.Info("migrations applied")
		return nil
	}

	m := metrics.New()
	rc := cache.New(cfg.RedisAddr, 0)
	defer func() { _ = rc.Close() }()

	network, err := geo.NewGrid(geo.DefaultGridConfig())
	if err != nil {
		return err
	}
	router := geo.NewRouter(network, cfg.RouteCacheSize)
	router.OnRoute = m.AStar

	dcfg := dispatch.DefaultConfig()
	dcfg.BatchWindow, dcfg.BatchMax = cfg.BatchWindow, cfg.BatchMax
	dcfg.RadiusM, dcfg.ExpandM, dcfg.TopK = cfg.SearchRadiusM, cfg.ExpandRadiusM, cfg.TopK
	dcfg.HeartbeatMaxAge, dcfg.DecisionTimeout, dcfg.MatchTimeout = cfg.HeartbeatMaxAge, cfg.DecisionTimeout, cfg.MatchTimeout
	dcfg.MaxHungarian, dcfg.LockTTL = cfg.MaxHungarian, cfg.LockTTL
	dcfg.Workers, dcfg.QueueSize = cfg.DispatchWorkers, cfg.QueueSize
	dispatcher := dispatch.New(dcfg, st, rc, router, m, log)

	publisher := &outbox.Publisher{
		Pool:     st.Pool(),
		Sink:     dispatcher.Enqueue,
		Poll:     cfg.OutboxPoll,
		Workers:  cfg.OutboxWorkers,
		Observer: m,
		Log:      log,
	}

	srv := &api.Server{
		Store:                  st,
		Cache:                  rc,
		Dispatcher:             dispatcher,
		Observer:               m,
		Log:                    log,
		HeartbeatWriteInterval: cfg.HeartbeatWrite,
		Extra:                  map[string]http.Handler{"/metrics": m.Handler()},
	}

	var wg sync.WaitGroup
	start := func(f func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f(ctx)
		}()
	}
	start(dispatcher.Run)
	start(publisher.Run)
	start(func(ctx context.Context) { cleanupIdempotencyKeys(ctx, st, cfg.IdempotencyTTL, log) })
	start(func(ctx context.Context) { sampleGauges(ctx, rc, dispatcher, m) })

	httpSrv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           srv.Router(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", cfg.HTTPAddr)
		errc <- httpSrv.ListenAndServe()
	}()
	select {
	case err = <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		stop()
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if serr := httpSrv.Shutdown(shutdownCtx); serr != nil && err == nil {
		err = serr
	}
	wg.Wait()
	return err
}

func cleanupIdempotencyKeys(ctx context.Context, st *store.Store, ttl time.Duration, log *slog.Logger) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		n, err := st.DeleteExpiredIdempotencyKeys(ctx, ttl)
		if err != nil && ctx.Err() == nil {
			log.Warn("idempotency cleanup failed", "err", err)
		} else if n > 0 {
			log.Info("idempotency keys expired", "deleted", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func sampleGauges(ctx context.Context, rc *cache.Cache, d *dispatch.Service, m *metrics.Metrics) {
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		if n, err := rc.AvailableCount(ctx); err == nil {
			m.AvailableVehicles(n)
		}
		m.QueueDepth(d.QueueDepth())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
