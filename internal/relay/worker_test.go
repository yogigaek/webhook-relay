package relay

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/yogigaek/webhook-relay/internal/signature"
	"github.com/yogigaek/webhook-relay/internal/store"
)

var testSecret = []byte("relay-to-internal-secret-32bytes")

// fakeQueue hands out its deliveries once and records what the worker decided for each.
type fakeQueue struct {
	mu        sync.Mutex
	pending   []store.Delivery
	delivered []int64
	retried   map[int64]time.Duration
	buried    map[int64]string
}

func newFakeQueue(ds ...store.Delivery) *fakeQueue {
	return &fakeQueue{pending: ds, retried: map[int64]time.Duration{}, buried: map[int64]string{}}
}

func (q *fakeQueue) ClaimDue(_ context.Context, limit int, _ time.Duration) ([]store.Delivery, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := min(limit, len(q.pending))
	out := q.pending[:n]
	q.pending = q.pending[n:]
	return out, nil
}

func (q *fakeQueue) MarkDelivered(_ context.Context, id int64) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.delivered = append(q.delivered, id)
	return nil
}

func (q *fakeQueue) Retry(_ context.Context, id int64, delay time.Duration, _ string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.retried[id] = delay
	return nil
}

func (q *fakeQueue) Bury(_ context.Context, id int64, reason string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.buried[id] = reason
	return nil
}

func (q *fakeQueue) outcome(id int64) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, d := range q.delivered {
		if d == id {
			return "delivered"
		}
	}
	if _, ok := q.retried[id]; ok {
		return "retried"
	}
	if _, ok := q.buried[id]; ok {
		return "buried"
	}
	return "none"
}

func newWorker(q Queue, url string) *Worker {
	return &Worker{
		Queue:        q,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Client:       &http.Client{Timeout: 200 * time.Millisecond},
		URL:          url,
		Secret:       testSecret,
		MaxAttempts:  3,
		PollInterval: 10 * time.Millisecond,
		BatchSize:    10,
		Lease:        time.Minute,
		Backoff:      func(attempt int) time.Duration { return time.Duration(attempt) * time.Second },
	}
}

func delivery(attempt int) store.Delivery {
	return store.Delivery{ID: 1, Provider: "acme-pay", EventID: "evt_1", Payload: []byte(`{"id":"evt_1"}`), Attempt: attempt}
}

func TestDeliveryOutcome(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		attempt int
		want    string
	}{
		{"2xx is delivered", http.StatusNoContent, 1, "delivered"},
		{"5xx is retried", http.StatusServiceUnavailable, 1, "retried"},
		{"429 is retried", http.StatusTooManyRequests, 1, "retried"},
		{"408 is retried", http.StatusRequestTimeout, 1, "retried"},
		{"other 4xx goes to dead letter at once", http.StatusBadRequest, 1, "buried"},
		{"5xx on the last attempt goes to dead letter", http.StatusInternalServerError, 3, "buried"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
			}))
			defer dest.Close()
			q := newFakeQueue()

			newWorker(q, dest.URL).handle(context.Background(), delivery(tt.attempt))

			if got := q.outcome(1); got != tt.want {
				t.Errorf("outcome = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestRetryUsesBackoff(t *testing.T) {
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer dest.Close()
	q := newFakeQueue()

	newWorker(q, dest.URL).handle(context.Background(), delivery(2))

	if got := q.retried[1]; got != 2*time.Second {
		t.Errorf("retry delay = %v, want Backoff(2) = 2s", got)
	}
}

// The destination checks the relay's signature the same way the relay checks providers.
func TestDeliverySignedAndLabelled(t *testing.T) {
	var got *http.Request
	var body []byte
	dest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		body, _ = io.ReadAll(r.Body)
	}))
	defer dest.Close()

	newWorker(newFakeQueue(), dest.URL).handle(context.Background(), delivery(1))

	if got == nil {
		t.Fatal("destination not called")
	}
	if err := signature.Verify(testSecret, got.Header.Get(signature.Header), body, time.Now(), time.Minute); err != nil {
		t.Errorf("signature does not verify: %v", err)
	}
	for header, want := range map[string]string{
		"Content-Type":     "application/json",
		"X-Relay-Provider": "acme-pay",
		"X-Relay-Event-Id": "evt_1",
		"X-Relay-Attempt":  "1",
	} {
		if v := got.Header.Get(header); v != want {
			t.Errorf("%s = %q, want %q", header, v, want)
		}
	}
}

func TestDestinationUnreachable(t *testing.T) {
	dest := httptest.NewServer(http.NotFoundHandler())
	url := dest.URL
	dest.Close() // nothing listens on url any more
	q := newFakeQueue()

	newWorker(q, url).handle(context.Background(), delivery(1))

	if got := q.outcome(1); got != "retried" {
		t.Errorf("outcome = %s, want retried", got)
	}
}

func TestDestinationTimeout(t *testing.T) {
	release := make(chan struct{})
	dest := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-release }))
	defer dest.Close()
	defer close(release)
	q := newFakeQueue()

	newWorker(q, dest.URL).handle(context.Background(), delivery(1)) // client timeout is 200ms

	if got := q.outcome(1); got != "retried" {
		t.Errorf("outcome = %s, want retried", got)
	}
}

// A delivery cut off by shutdown records nothing; the lease brings the event back after restart.
func TestShutdownMidDelivery(t *testing.T) {
	arrived := make(chan struct{})
	release := make(chan struct{})
	dest := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(arrived)
		<-release
	}))
	defer dest.Close()
	defer close(release)
	q := newFakeQueue()
	w := newWorker(q, dest.URL)
	w.Client.Timeout = 5 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		w.handle(ctx, delivery(1))
		close(done)
	}()
	<-arrived
	cancel()
	<-done

	if got := q.outcome(1); got != "none" {
		t.Errorf("outcome = %s, want none", got)
	}
}

func TestRunDeliversEverythingAndStops(t *testing.T) {
	dest := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer dest.Close()
	var ds []store.Delivery
	for i := range 25 { // more than one batch of 10
		ds = append(ds, store.Delivery{ID: int64(i + 1), Provider: "acme-pay", EventID: "evt", Payload: []byte(`{}`), Attempt: 1})
	}
	q := newFakeQueue(ds...)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		newWorker(q, dest.URL).Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for {
		q.mu.Lock()
		n := len(q.delivered)
		q.mu.Unlock()
		if n == len(ds) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d delivered before the deadline", n, len(ds))
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestExponentialBackoff(t *testing.T) {
	backoff := ExponentialBackoff(10*time.Second, 5*time.Minute)
	tests := []struct {
		attempt int
		full    time.Duration // the delay before jitter
	}{
		{1, 10 * time.Second},
		{2, 20 * time.Second},
		{3, 40 * time.Second},
		{6, 5 * time.Minute}, // 320s, capped
		{100, 5 * time.Minute},
	}
	for _, tt := range tests {
		for range 50 {
			got := backoff(tt.attempt)
			if got < tt.full/2 || got > tt.full {
				t.Fatalf("attempt %d: delay %v outside [%v, %v]", tt.attempt, got, tt.full/2, tt.full)
			}
		}
	}
}
