// Command api runs the dispatch HTTP service.
//
//	api           serve HTTP
//	api migrate   apply SQL migrations and exit
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yulo-dev/robotaxi-dispatch/internal/api"
	"github.com/yulo-dev/robotaxi-dispatch/internal/config"
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

	srv := &api.Server{
		Store:                  st,
		Log:                    log,
		HeartbeatWriteInterval: cfg.HeartbeatWrite,
	}
	go cleanupIdempotencyKeys(ctx, st, cfg.IdempotencyTTL, log)

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
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return httpSrv.Shutdown(shutdownCtx)
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
