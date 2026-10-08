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

	"github.com/exaring/otelpgx"
	"github.com/jackc/pgx/v5/pgxpool"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/yogigaek/webhook-relay/internal/config"
	"github.com/yogigaek/webhook-relay/internal/httpapi"
	"github.com/yogigaek/webhook-relay/internal/relay"
	"github.com/yogigaek/webhook-relay/internal/store"
	"github.com/yogigaek/webhook-relay/internal/telemetry"
)

func main() {
	logger := slog.New(telemetry.LogHandler{Handler: slog.NewJSONHandler(os.Stdout, nil)})
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

	shutdownTracing, err := telemetry.Setup(startCtx, "webhook-relay")
	if err != nil {
		return fmt.Errorf("telemetry: %w", err)
	}
	// Last to run: flushes the spans of everything that happened during shutdown too.
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := shutdownTracing(ctx); err != nil {
			logger.Warn("flush traces", "error", err)
		}
	}()

	poolConfig, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("database config: %w", err)
	}
	// every query becomes a span under the request or delivery that ran it
	poolConfig.ConnConfig.Tracer = otelpgx.NewTracer()
	pool, err := pgxpool.NewWithConfig(startCtx, poolConfig)
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

	const (
		deliveryTimeout = 10 * time.Second
		batchSize       = 20
	)
	worker := &relay.Worker{
		Queue:  store.New(pool),
		Logger: logger.With("component", "relay"),
		// The otelhttp transport adds a client span and sends traceparent, so the destination's own
		// spans join the delivery trace.
		Client:       &http.Client{Timeout: deliveryTimeout, Transport: otelhttp.NewTransport(http.DefaultTransport)},
		URL:          cfg.DeliveryURL,
		Secret:       cfg.DeliverySecret,
		MaxAttempts:  cfg.MaxAttempts,
		PollInterval: time.Second,
		BatchSize:    batchSize,
		// long enough for a full batch of timed-out deliveries, plus room for the database writes
		Lease:   batchSize*deliveryTimeout + time.Minute,
		Backoff: relay.ExponentialBackoff(10*time.Second, time.Hour),
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
