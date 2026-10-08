// Command webhook-relay receives payment provider webhooks and relays them to internal services.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogigaek/webhook-relay/internal/config"
	"github.com/yogigaek/webhook-relay/internal/httpapi"
	"github.com/yogigaek/webhook-relay/internal/relay"
	"github.com/yogigaek/webhook-relay/internal/store"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(logger); err != nil {
		logger.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return err
	}

	startCtx, cancelStart := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelStart()
	pool, err := pgxpool.New(startCtx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database config: %w", err)
	}
	defer pool.Close()
	// pgxpool connects lazily; ping now so a wrong URL or a database that is down fails the start
	if err := pool.Ping(startCtx); err != nil {
		return fmt.Errorf("database unreachable: %w", err)
	}
	if err := store.Migrate(startCtx, pool); err != nil {
		return err
	}

	server := &http.Server{
		Addr: cfg.Addr,
		Handler: httpapi.NewHandler(httpapi.Options{
			Logger:             logger,
			Store:              store.New(pool),
			Secrets:            cfg.Secrets,
			SignatureTolerance: cfg.SignatureTolerance,
		}),
		// Timeouts stop a slow or stalled client from holding a connection open forever.
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Ctrl+C locally, SIGTERM from Docker: both start a graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	worker := &relay.Worker{
		Queue:  store.New(pool),
		Logger: logger.With("component", "relay"),
		// The timeout bounds one delivery; the lease below must stay longer than it.
		Client:       &http.Client{Timeout: 10 * time.Second},
		URL:          cfg.DeliveryURL,
		Secret:       cfg.DeliverySecret,
		MaxAttempts:  cfg.MaxAttempts,
		PollInterval: time.Second,
		BatchSize:    20,
		Lease:        time.Minute,
		Backoff:      relay.ExponentialBackoff(10*time.Second, time.Hour),
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	go func() {
		defer close(workerDone)
		worker.Run(workerCtx)
	}()
	// On every way out of run, the worker finishes its current delivery before the pool closes.
	defer func() {
		stopWorker()
		<-workerDone
	}()

	serveErr := make(chan error, 1)
	go func() {
		providers := make([]string, 0, len(cfg.Secrets))
		for name := range cfg.Secrets {
			providers = append(providers, name)
		}
		logger.Info("listening", "addr", cfg.Addr, "providers", providers)
		serveErr <- server.ListenAndServe()
	}()

	select {
	case err := <-serveErr:
		// The server never started (port in use, for example).
		return err
	case <-ctx.Done():
	}

	logger.Info("shutting down")
	// Requests already in flight get up to 10 seconds to finish; new ones are refused.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-serveErr; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	stopWorker()
	<-workerDone
	logger.Info("stopped")
	return nil
}
