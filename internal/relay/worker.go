// Package relay delivers stored webhook events to the internal destination, retrying failures with
// exponential backoff and moving events that cannot be delivered to the dead-letter state.
package relay

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"

	"github.com/yogigaek/webhook-relay/internal/signature"
	"github.com/yogigaek/webhook-relay/internal/store"
)

// Queue is the part of the store the worker needs; tests use an in-memory fake.
type Queue interface {
	ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]store.Delivery, error)
	MarkDelivered(ctx context.Context, id int64, attempt int) error
	Retry(ctx context.Context, id int64, attempt int, delay time.Duration, reason string) error
	Bury(ctx context.Context, id int64, attempt int, reason string) error
}

type Worker struct {
	Queue  Queue
	Logger *slog.Logger
	Client *http.Client
	// URL receives every event as a POST, signed with Secret in the same format the relay accepts.
	URL    string
	Secret []byte

	MaxAttempts  int
	PollInterval time.Duration
	BatchSize    int
	// Lease must outlast a whole batch, BatchSize deliveries back to back, each taking up to the
	// client timeout plus OutcomeWriteTimeout: deliveries run one after another, and an event whose
	// lease ran out can be claimed by another worker while this one is still on its way to it.
	Lease time.Duration
	// Backoff gives the delay before the next attempt after the given failed attempt.
	Backoff func(attempt int) time.Duration
}

// Run polls for due events until ctx is cancelled, then returns once the current batch is done.
func (w *Worker) Run(ctx context.Context) {
	ticker := time.NewTicker(w.PollInterval)
	defer ticker.Stop()
	for {
		// Drain: while a batch comes back full there is likely more waiting, so claim again at once.
		for w.processBatch(ctx) == w.BatchSize && ctx.Err() == nil {
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// processBatch claims and delivers one batch, returning how many events it claimed.
func (w *Worker) processBatch(ctx context.Context) int {
	deliveries, err := w.Queue.ClaimDue(ctx, w.BatchSize, w.Lease)
	if err != nil {
		if ctx.Err() == nil {
			w.Logger.ErrorContext(ctx, "claim events", "error", err)
		}
		return 0
	}
	for _, d := range deliveries {
		w.handle(ctx, d)
	}
	return len(deliveries)
}

// OutcomeWriteTimeout bounds recording the result of one delivery; it counts toward the lease.
const OutcomeWriteTimeout = 5 * time.Second

// The tracer is looked up per delivery rather than once at package load: a package-level tracer
// stays bound to whichever global provider existed first.
const tracerName = "github.com/yogigaek/webhook-relay/internal/relay"

func (w *Worker) handle(ctx context.Context, d store.Delivery) {
	// Each delivery is its own trace (it happens seconds or hours after the webhook arrived), linked
	// to the trace of the request that received the event: a trace UI can jump from one to the other.
	var links []trace.Link
	if d.TraceParent != "" {
		received := propagation.TraceContext{}.Extract(context.Background(), propagation.MapCarrier{"traceparent": d.TraceParent})
		if sc := trace.SpanContextFromContext(received); sc.IsValid() {
			links = append(links, trace.Link{SpanContext: sc})
		}
	}
	ctx, span := otel.Tracer(tracerName).Start(ctx, "relay.deliver",
		trace.WithLinks(links...),
		trace.WithAttributes(
			attribute.String("webhook.provider", d.Provider),
			attribute.String("webhook.event_id", d.EventID),
			attribute.Int("relay.attempt", d.Attempt),
		),
	)
	defer span.End()

	log := w.Logger.With("provider", d.Provider, "event_id", d.EventID, "attempt", d.Attempt)

	err := w.deliver(ctx, d)
	if err != nil {
		span.SetStatus(codes.Error, err.Error())
	}
	if err != nil && ctx.Err() != nil {
		// Shutting down cut the delivery off: record nothing. The lease runs out and the next start
		// delivers the event again, so it is late but never lost.
		log.InfoContext(ctx, "delivery interrupted by shutdown")
		span.SetAttributes(attribute.String("relay.outcome", "interrupted"))
		return
	}

	// The outcome is written with a fresh context so a shutdown starting right now cannot drop
	// the result of a delivery that already happened.
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), OutcomeWriteTimeout)
	defer cancel()

	var (
		permanent *permanentError
		outcome   string
		retryIn   time.Duration
		recordErr error
	)
	switch {
	case err == nil:
		outcome = "delivered"
		recordErr = w.Queue.MarkDelivered(writeCtx, d.ID, d.Attempt)
	case errors.As(err, &permanent) || d.Attempt >= w.MaxAttempts:
		outcome = "dead"
		recordErr = w.Queue.Bury(writeCtx, d.ID, d.Attempt, err.Error())
	default:
		outcome = "retry"
		retryIn = w.Backoff(d.Attempt)
		recordErr = w.Queue.Retry(writeCtx, d.ID, d.Attempt, retryIn, err.Error())
	}

	switch {
	case errors.Is(recordErr, store.ErrLeaseLost):
		// Another attempt owns the event now and will record its own result.
		log.WarnContext(ctx, "outcome discarded: lease lost", "outcome", outcome)
		outcome = "lease_lost"
	case recordErr != nil:
		// The lease brings the event back, so it is delivered again rather than lost.
		log.ErrorContext(ctx, "record outcome", "outcome", outcome, "error", recordErr)
		outcome = "unrecorded"
	case outcome == "delivered":
		log.InfoContext(ctx, "event delivered")
	case outcome == "dead":
		log.WarnContext(ctx, "event moved to dead letter", "reason", err.Error())
	default:
		log.WarnContext(ctx, "delivery failed, will retry", "reason", err.Error(), "retry_in", retryIn.String())
		span.SetAttributes(attribute.String("relay.retry_in", retryIn.String()))
	}
	span.SetAttributes(attribute.String("relay.outcome", outcome))
}

// permanentError marks a failure that retrying cannot fix.
type permanentError struct{ msg string }

func (e *permanentError) Error() string { return e.msg }

func (w *Worker) deliver(ctx context.Context, d store.Delivery) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.URL, bytes.NewReader(d.Payload))
	if err != nil {
		return &permanentError{msg: "build request: " + err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(signature.Header, signature.Sign(w.Secret, time.Now(), d.Payload))
	req.Header.Set("X-Relay-Provider", d.Provider)
	req.Header.Set("X-Relay-Event-Id", d.EventID)
	req.Header.Set("X-Relay-Attempt", strconv.Itoa(d.Attempt))

	resp, err := w.Client.Do(req)
	if err != nil {
		// network error or timeout: the destination may be restarting, so try again later
		return err
	}
	defer resp.Body.Close()
	// reading the (small) rest of the body lets the connection be reused
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusRequestTimeout || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("status %d", resp.StatusCode)
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// The destination understood the request and refused it; sending the same bytes again
		// gets the same answer. Dead-letter it so a person can look.
		return &permanentError{msg: fmt.Sprintf("status %d", resp.StatusCode)}
	default:
		return fmt.Errorf("status %d", resp.StatusCode)
	}
}

// ExponentialBackoff doubles the delay after every failed attempt, from base up to ceiling, with
// jitter: after an outage, events that failed together do not all come back in the same second.
func ExponentialBackoff(base, ceiling time.Duration) func(attempt int) time.Duration {
	return func(attempt int) time.Duration {
		attempt = max(attempt, 1) // attempts start at 1; a lower value must not make a negative shift
		delay := ceiling
		if attempt < 30 { // past this, base << attempt would overflow
			if d := base << (attempt - 1); d > 0 && d < ceiling {
				delay = d
			}
		}
		// "equal jitter": half fixed, half random, so a delay never collapses to near zero
		half := delay / 2
		return half + rand.N(half+1)
	}
}
