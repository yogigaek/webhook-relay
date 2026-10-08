package httpapi

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/yogigaek/webhook-relay/internal/signature"
	"github.com/yogigaek/webhook-relay/internal/store"
)

var (
	testSecret = []byte("0123456789abcdef0123456789abcdef")
	testNow    = time.Unix(1_800_000_000, 0)
)

// fakeStore keeps events in memory with the same idempotency rule as the real store.
type fakeStore struct {
	mu   sync.Mutex
	seen map[string]bool
	last store.Event
	err  error // returned by every call when set
}

func (f *fakeStore) Save(_ context.Context, e store.Event) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return false, f.err
	}
	key := e.Provider + "/" + e.EventID
	if f.seen[key] {
		return false, nil
	}
	f.seen[key] = true
	f.last = e
	return true, nil
}

func (f *fakeStore) Ping(context.Context) error { return f.err }

func newTestHandler(s *fakeStore) http.Handler {
	return NewHandler(Options{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		Store:              s,
		Secrets:            map[string][]byte{"acme-pay": testSecret},
		SignatureTolerance: 5 * time.Minute,
		Now:                func() time.Time { return testNow },
	})
}

func signed(body string) string { return signature.Sign(testSecret, testNow, []byte(body)) }

func postWebhook(h http.Handler, provider, contentType, sig, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/webhooks/"+provider, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if sig != "" {
		req.Header.Set(signature.Header, sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestWebhook(t *testing.T) {
	large := `"` + strings.Repeat("a", MaxBodyBytes) + `"`
	longID := `{"id":"` + strings.Repeat("x", maxEventIDLength+1) + `"}`

	tests := []struct {
		name        string
		provider    string
		contentType string
		sig         string
		body        string
		wantStatus  int
	}{
		{"valid webhook", "acme-pay", "application/json", signed(`{"id":"evt_1"}`), `{"id":"evt_1"}`, http.StatusAccepted},
		{"content type with charset", "acme-pay", "application/json; charset=utf-8", signed(`{"id":"evt_2"}`), `{"id":"evt_2"}`, http.StatusAccepted},
		{"missing signature", "acme-pay", "application/json", "", `{"id":"evt_1"}`, http.StatusUnauthorized},
		{"signature for another body", "acme-pay", "application/json", signed(`{"id":"evt_1","amount":1}`), `{"id":"evt_1","amount":1000}`, http.StatusUnauthorized},
		{"expired signature", "acme-pay", "application/json", signature.Sign(testSecret, testNow.Add(-time.Hour), []byte(`{"id":"evt_1"}`)), `{"id":"evt_1"}`, http.StatusUnauthorized},
		{"signed but invalid JSON", "acme-pay", "application/json", signed(`{"id":`), `{"id":`, http.StatusBadRequest},
		{"no id", "acme-pay", "application/json", signed(`{"type":"paid"}`), `{"type":"paid"}`, http.StatusBadRequest},
		{"numeric id", "acme-pay", "application/json", signed(`{"id":42}`), `{"id":42}`, http.StatusBadRequest},
		{"array body", "acme-pay", "application/json", signed(`[]`), `[]`, http.StatusBadRequest},
		{"id too long", "acme-pay", "application/json", signed(longID), longID, http.StatusBadRequest},
		{"wrong content type", "acme-pay", "text/plain", signed(`{"id":"evt_1"}`), `{"id":"evt_1"}`, http.StatusUnsupportedMediaType},
		{"unknown provider", "other-pay", "application/json", signed(`{"id":"evt_1"}`), `{"id":"evt_1"}`, http.StatusNotFound},
		{"payload too large", "acme-pay", "application/json", signed(large), large, http.StatusRequestEntityTooLarge},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler(&fakeStore{seen: map[string]bool{}})
			rec := postWebhook(h, tt.provider, tt.contentType, tt.sig, tt.body)
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

func TestWebhookDuplicate(t *testing.T) {
	h := newTestHandler(&fakeStore{seen: map[string]bool{}})
	body := `{"id":"evt_1"}`

	if rec := postWebhook(h, "acme-pay", "application/json", signed(body), body); rec.Code != http.StatusAccepted {
		t.Fatalf("first delivery status = %d, want 202", rec.Code)
	}
	rec := postWebhook(h, "acme-pay", "application/json", signed(body), body)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "duplicate") {
		t.Errorf("retry status = %d body %s, want 200 duplicate", rec.Code, rec.Body.String())
	}
}

func TestWebhookUnstorablePayload(t *testing.T) {
	h := newTestHandler(&fakeStore{err: store.ErrInvalidPayload})
	body := `{"id":"evt_1"}`
	if rec := postWebhook(h, "acme-pay", "application/json", signed(body), body); rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 so the provider does not retry", rec.Code)
	}
}

func TestWebhookStoreDown(t *testing.T) {
	h := newTestHandler(&fakeStore{err: errors.New("connection refused")})
	body := `{"id":"evt_1"}`
	rec := postWebhook(h, "acme-pay", "application/json", signed(body), body)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 so the provider retries", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "connection refused") {
		t.Errorf("response leaks the internal error: %s", rec.Body.String())
	}
}

// A rejected request must not tell the caller why: "expired" vs "mismatch" would help a forger.
func TestRejectionsLookAlike(t *testing.T) {
	h := newTestHandler(&fakeStore{seen: map[string]bool{}})
	body := `{"id":"evt_1"}`
	bodies := map[string]string{}
	for name, sig := range map[string]string{
		"missing":  "",
		"expired":  signature.Sign(testSecret, testNow.Add(-time.Hour), []byte(body)),
		"mismatch": signature.Sign([]byte("another-secret-0123456789abcdef!"), testNow, []byte(body)),
	} {
		bodies[name] = postWebhook(h, "acme-pay", "application/json", sig, body).Body.String()
	}
	if bodies["missing"] != bodies["expired"] || bodies["expired"] != bodies["mismatch"] {
		t.Errorf("rejection bodies differ: %v", bodies)
	}
}

func TestProbes(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		storeErr   error
		wantStatus int
	}{
		{"health", http.MethodGet, "/healthz", nil, http.StatusOK},
		{"health ignores the database", http.MethodGet, "/healthz", errors.New("down"), http.StatusOK},
		{"ready", http.MethodGet, "/readyz", nil, http.StatusOK},
		{"not ready without database", http.MethodGet, "/readyz", errors.New("down"), http.StatusServiceUnavailable},
		{"GET on webhook route", http.MethodGet, "/webhooks/acme-pay", nil, http.StatusMethodNotAllowed},
		{"unknown path", http.MethodGet, "/nope", nil, http.StatusNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHandler(&fakeStore{seen: map[string]bool{}, err: tt.storeErr})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(tt.method, tt.path, nil))
			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
		})
	}
}

// An event received inside a trace keeps that trace's id, so its later delivery can link back.
func TestWebhookStoresTraceParent(t *testing.T) {
	otel.SetTracerProvider(sdktrace.NewTracerProvider())
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() {
		otel.SetTracerProvider(noop.NewTracerProvider())
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator())
	})

	s := &fakeStore{seen: map[string]bool{}}
	body := `{"id":"evt_1"}`
	req := httptest.NewRequest(http.MethodPost, "/webhooks/acme-pay", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(signature.Header, signed(body))
	// the provider's own trace, as a caller that propagates W3C trace context would send it
	req.Header.Set("traceparent", "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01")
	rec := httptest.NewRecorder()

	newTestHandler(s).ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d", rec.Code)
	}
	if !strings.HasPrefix(s.last.TraceParent, "00-4bf92f3577b34da6a3ce929d0e0e4736-") {
		t.Errorf("stored traceparent %q does not continue the incoming trace", s.last.TraceParent)
	}
}
