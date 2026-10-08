// Command sink stands in for the internal service that receives relayed events. It checks the
// relay's signature and logs each event, so `docker compose --profile demo up` shows the whole path.
// It is a demo, not part of the relay.
package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/yogigaek/webhook-relay/internal/signature"
	"github.com/yogigaek/webhook-relay/internal/telemetry"
)

func main() {
	logger := slog.New(telemetry.LogHandler{Handler: slog.NewJSONHandler(os.Stdout, nil)})
	// The relay sends traceparent, so the sink's span lands in the same trace as the delivery.
	if _, err := telemetry.Setup(context.Background(), "sink"); err != nil {
		logger.Error("telemetry", "error", err)
		os.Exit(1)
	}
	secret := []byte(os.Getenv("DELIVERY_SECRET"))
	if len(secret) == 0 {
		logger.Error("DELIVERY_SECRET is empty")
		os.Exit(1)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /events", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		if err := signature.Verify(secret, r.Header.Get(signature.Header), body, time.Now(), 5*time.Minute); err != nil {
			logger.WarnContext(r.Context(), "rejected", "reason", err.Error())
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		logger.InfoContext(r.Context(), "event received",
			"provider", r.Header.Get("X-Relay-Provider"),
			"event_id", r.Header.Get("X-Relay-Event-Id"),
			"attempt", r.Header.Get("X-Relay-Attempt"),
			"payload", string(body),
		)
		w.WriteHeader(http.StatusNoContent)
	})

	server := &http.Server{Addr: ":9000", Handler: otelhttp.NewHandler(mux, "sink"), ReadHeaderTimeout: 5 * time.Second}
	logger.Info("sink listening", "addr", server.Addr)
	if err := server.ListenAndServe(); err != nil {
		logger.Error("sink stopped", "error", err)
		os.Exit(1)
	}
}
