// Package httpapi exposes the HTTP endpoints of the relay.
package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"github.com/yogigaek/webhook-relay/internal/signature"
)

// MaxBodyBytes caps a webhook payload. Providers send small JSON documents; anything larger is
// rejected before it is read into memory.
const MaxBodyBytes = 1 << 20 // 1 MiB

type Options struct {
	Logger *slog.Logger
	// Secrets maps a provider name to its signing secret; any other provider gets 404.
	Secrets            map[string][]byte
	SignatureTolerance time.Duration
	// Now is the clock used to check signature timestamps; tests pass a fixed time.
	Now func() time.Time
}

// NewHandler returns the router with every route the relay serves.
func NewHandler(opts Options) http.Handler {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("POST /webhooks/{provider}", handleWebhook(opts))
	return mux
}

func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleWebhook(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provider := r.PathValue("provider")
		secret, known := opts.Secrets[provider]
		if !known {
			writeError(w, http.StatusNotFound, "unknown provider")
			return
		}

		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/json" {
			writeError(w, http.StatusUnsupportedMediaType, "content type must be application/json")
			return
		}

		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				writeError(w, http.StatusRequestEntityTooLarge, "payload too large")
				return
			}
			writeError(w, http.StatusBadRequest, "could not read body")
			return
		}

		// Verified against the raw bytes, before any parsing: re-encoded JSON would not match.
		err = signature.Verify(secret, r.Header.Get(signature.Header), body, opts.Now(), opts.SignatureTolerance)
		if err != nil {
			// The caller only learns that the signature was rejected; the reason stays in the log
			// so a forger gets no hint about which part to fix.
			opts.Logger.WarnContext(r.Context(), "webhook rejected", "provider", provider, "reason", err.Error())
			writeError(w, http.StatusUnauthorized, "invalid signature")
			return
		}

		if !json.Valid(body) {
			writeError(w, http.StatusBadRequest, "body is not valid JSON")
			return
		}

		// Storage (stage 3) happens before this response once it exists.
		opts.Logger.InfoContext(r.Context(), "webhook received", "provider", provider, "bytes", len(body))
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "accepted"})
	}
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
