// Command webhook-relay receives payment provider webhooks and relays them to internal services.
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

	"github.com/yogigaek/webhook-relay/internal/config"
	"github.com/yogigaek/webhook-relay/internal/httpapi"
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

	server := &http.Server{
		Addr: cfg.Addr,
		Handler: httpapi.NewHandler(httpapi.Options{
			Logger:             logger,
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
	logger.Info("stopped")
	return nil
}
