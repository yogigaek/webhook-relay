package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yogigaek/webhook-relay/internal/signature"
)

var (
	testSecret = []byte("0123456789abcdef0123456789abcdef")
	testNow    = time.Unix(1_800_000_000, 0)
)

func newTestHandler() http.Handler {
	return NewHandler(Options{
		Logger:             slog.New(slog.NewTextHandler(io.Discard, nil)),
		Secrets:            map[string][]byte{"acme-pay": testSecret},
		SignatureTolerance: 5 * time.Minute,
		Now:                func() time.Time { return testNow },
	})
}

func TestRoutes(t *testing.T) {
	handler := newTestHandler()
	signed := func(body string) string { return signature.Sign(testSecret, testNow, []byte(body)) }
	large := `"` + strings.Repeat("a", MaxBodyBytes) + `"`

	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		sig         string
		body        string
		wantStatus  int
	}{
		{"health check", http.MethodGet, "/healthz", "", "", "", http.StatusOK},
		{"valid webhook", http.MethodPost, "/webhooks/acme-pay", "application/json", signed(`{"id":"evt_1"}`), `{"id":"evt_1"}`, http.StatusAccepted},
		{"content type with charset", http.MethodPost, "/webhooks/acme-pay", "application/json; charset=utf-8", signed(`{}`), `{}`, http.StatusAccepted},
		{"missing signature", http.MethodPost, "/webhooks/acme-pay", "application/json", "", `{}`, http.StatusUnauthorized},
		{"signature for another body", http.MethodPost, "/webhooks/acme-pay", "application/json", signed(`{"amount":1}`), `{"amount":1000}`, http.StatusUnauthorized},
		{"expired signature", http.MethodPost, "/webhooks/acme-pay", "application/json", signature.Sign(testSecret, testNow.Add(-time.Hour), []byte(`{}`)), `{}`, http.StatusUnauthorized},
		{"signed but invalid JSON", http.MethodPost, "/webhooks/acme-pay", "application/json", signed(`{"id":`), `{"id":`, http.StatusBadRequest},
		{"wrong content type", http.MethodPost, "/webhooks/acme-pay", "text/plain", signed(`{}`), `{}`, http.StatusUnsupportedMediaType},
		{"unknown provider", http.MethodPost, "/webhooks/other-pay", "application/json", signed(`{}`), `{}`, http.StatusNotFound},
		{"payload too large", http.MethodPost, "/webhooks/acme-pay", "application/json", signed(large), large, http.StatusRequestEntityTooLarge},
		{"GET on webhook route", http.MethodGet, "/webhooks/acme-pay", "", "", "", http.StatusMethodNotAllowed},
		{"unknown path", http.MethodGet, "/nope", "", "", "", http.StatusNotFound},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			if tt.contentType != "" {
				req.Header.Set("Content-Type", tt.contentType)
			}
			if tt.sig != "" {
				req.Header.Set(signature.Header, tt.sig)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body: %s)", rec.Code, tt.wantStatus, rec.Body.String())
			}
		})
	}
}

// A rejected request must not tell the caller why: "expired" vs "mismatch" would help a forger.
func TestRejectionsLookAlike(t *testing.T) {
	handler := newTestHandler()
	bodies := map[string]string{}
	for name, sig := range map[string]string{
		"missing":  "",
		"expired":  signature.Sign(testSecret, testNow.Add(-time.Hour), []byte(`{}`)),
		"mismatch": signature.Sign([]byte("another-secret-0123456789abcdef!"), testNow, []byte(`{}`)),
	} {
		req := httptest.NewRequest(http.MethodPost, "/webhooks/acme-pay", strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(signature.Header, sig)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		bodies[name] = rec.Body.String()
	}
	if bodies["missing"] != bodies["expired"] || bodies["expired"] != bodies["mismatch"] {
		t.Errorf("rejection bodies differ: %v", bodies)
	}
}
