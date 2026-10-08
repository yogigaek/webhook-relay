// Package httpapi exposes the HTTP endpoints of the relay.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/yogigaek/webhook-relay/internal/signature"
	"github.com/yogigaek/webhook-relay/internal/store"
)

// MaxBodyBytes caps a webhook payload. Providers send small JSON documents; anything larger is
// rejected before it is read into memory.
const MaxBodyBytes = 1 << 20 // 1 MiB

// maxEventIDLength bounds the idempotency key that ends up in a unique index.
const maxEventIDLength = 255

// EventStore is the part of the store the handlers need. *store.Store satisfies it; unit tests
// use an in-memory fake, so they run without a database.
type EventStore interface {
	Save(ctx context.Context, e store.Event) (inserted bool, err error)
	Ping(ctx context.Context) error
}

type Options struct {
	Logger *slog.Logger
	Store  EventStore
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
	// Probes stay untraced: they run every few seconds and would bury the spans that matter.
	mux.HandleFunc("GET /healthz", handleHealth)
	mux.HandleFunc("GET /readyz", handleReady(opts))
	// The span is named after the route pattern, not the URL, so every provider shares one name.
	const webhookRoute = "POST /webhooks/{provider}"
	mux.Handle(webhookRoute, otelhttp.NewHandler(handleWebhook(opts), webhookRoute,
		// Anyone can call this route, so its trace context is not trusted: the span starts a trace of
		// our own (a caller cannot switch sampling off or pick the trace) and only links to theirs.
		otelhttp.WithPublicEndpointFn(func(*http.Request) bool { return true }),
		// no Baggage propagator: a caller's baggage is not let into the request context
		otelhttp.WithPropagators(propagation.TraceContext{}),
	))
	return mux
}

// handleHealth answers "is the process alive"; it never touches the database, so a database
// outage does not get the container restarted for nothing.
func handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleReady answers "can it take traffic", which needs the database.
func handleReady(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := opts.Store.Ping(ctx); err != nil {
			opts.Logger.WarnContext(r.Context(), "not ready", "error", err)
			writeError(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	}
}

func handleWebhook(opts Options) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provider := r.PathValue("provider")
		secret, known := opts.Secrets[provider]
		if !known {
			writeError(w, http.StatusNotFound, "unknown provider")
			return
		}
		// set only for configured providers: any string in the URL would otherwise reach the traces
		span := trace.SpanFromContext(r.Context())
		span.SetAttributes(attribute.String("webhook.provider", provider))

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

		// The provider's event id is the idempotency key, so every event must carry one.
		var envelope struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil || envelope.ID == "" || len(envelope.ID) > maxEventIDLength {
			writeError(w, http.StatusBadRequest, `body must be a JSON object with a string "id"`)
			return
		}

		span.SetAttributes(attribute.String("webhook.event_id", envelope.ID))

		// The worker delivers later, in its own trace; storing this request's traceparent lets
		// that delivery span link back here.
		carrier := propagation.MapCarrier{}
		propagation.TraceContext{}.Inject(r.Context(), carrier)

		inserted, err := opts.Store.Save(r.Context(), store.Event{
			Provider:    provider,
			EventID:     envelope.ID,
			Payload:     body,
			TraceParent: carrier.Get("traceparent"),
		})
		if errors.Is(err, store.ErrInvalidPayload) {
			// a retry would fail the same way, so tell the provider not to retry
			writeError(w, http.StatusBadRequest, "payload cannot be stored")
			return
		}
		if err != nil {
			// 500 makes the provider retry later, which is what should happen while the database
			// is down: the event is not lost, only delayed.
			opts.Logger.ErrorContext(r.Context(), "store event", "provider", provider, "event_id", envelope.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "could not store event")
			return
		}
		span.SetAttributes(attribute.Bool("webhook.duplicate", !inserted))
		if !inserted {
			// Still a success, so the provider stops retrying an event that is already stored.
			opts.Logger.InfoContext(r.Context(), "duplicate webhook ignored", "provider", provider, "event_id", envelope.ID)
			writeJSON(w, http.StatusOK, map[string]string{"status": "duplicate"})
			return
		}

		opts.Logger.InfoContext(r.Context(), "webhook stored", "provider", provider, "event_id", envelope.ID, "bytes", len(body))
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
