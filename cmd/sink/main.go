// Command sink stands in for the internal service that receives relayed events. It checks the
// relay's signature and logs each event, so `docker compose --profile demo up` shows the whole path.
// It is a demo, not part of the relay.
package main

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/yogigaek/webhook-relay/internal/signature"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
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
			logger.Warn("rejected", "reason", err.Error())
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		logger.Info("event received",
			"provider", r.Header.Get("X-Relay-Provider"),
			"event_id", r.Header.Get("X-Relay-Event-Id"),
			"attempt", r.Header.Get("X-Relay-Attempt"),
			"payload", string(body),
		)
		w.WriteHeader(http.StatusNoContent)
	})

	server := &http.Server{Addr: ":9000", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	logger.Info("sink listening", "addr", server.Addr)
	if err := server.ListenAndServe(); err != nil {
		logger.Error("sink stopped", "error", err)
		os.Exit(1)
	}
}
