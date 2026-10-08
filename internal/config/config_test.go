package config

import (
	"strings"
	"testing"
	"time"
)

const goodSecret = "0123456789abcdef0123456789abcdef" // exactly 32 characters

const testDatabaseURL = "postgres://relay:relay@localhost:5434/relay"

// defaults fills the required settings a test does not care about.
var defaults = map[string]string{
	"DATABASE_URL":    testDatabaseURL,
	"DELIVERY_URL":    "http://localhost:9000/events",
	"DELIVERY_SECRET": goodSecret,
}

// env serves vars, falling back to defaults; a test sets a key to "" to leave it out.
func env(vars map[string]string) func(string) string {
	return func(key string) string {
		if v, ok := vars[key]; ok {
			return v
		}
		return defaults[key]
	}
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(env(map[string]string{"PROVIDER_SECRETS": "acme-pay=" + goodSecret}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != ":8080" {
		t.Errorf("Addr = %q, want :8080", cfg.Addr)
	}
	if cfg.DatabaseURL != testDatabaseURL {
		t.Errorf("DatabaseURL = %q", cfg.DatabaseURL)
	}
	if cfg.SignatureTolerance != 5*time.Minute {
		t.Errorf("SignatureTolerance = %v, want 5m", cfg.SignatureTolerance)
	}
	if cfg.MaxAttempts != 8 {
		t.Errorf("MaxAttempts = %d, want 8", cfg.MaxAttempts)
	}
	if string(cfg.Secrets["acme-pay"]) != goodSecret {
		t.Errorf("secret for acme-pay not loaded")
	}
}

func TestLoadOverrides(t *testing.T) {
	cfg, err := Load(env(map[string]string{
		"PORT":                "9000",
		"SIGNATURE_TOLERANCE": "90s",
		"PROVIDER_SECRETS":    " acme-pay=" + goodSecret + " , other-bank=" + goodSecret + "==",
	}))
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.Addr != ":9000" || cfg.SignatureTolerance != 90*time.Second {
		t.Errorf("got Addr %q, tolerance %v", cfg.Addr, cfg.SignatureTolerance)
	}
	// only the first "=" separates name from secret, so base64 padding survives
	if string(cfg.Secrets["other-bank"]) != goodSecret+"==" {
		t.Errorf("secret for other-bank = %q", cfg.Secrets["other-bank"])
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name     string
		vars     map[string]string
		wantText string
	}{
		{"no database", map[string]string{"DATABASE_URL": "", "PROVIDER_SECRETS": "a=" + goodSecret}, "DATABASE_URL is empty"},
		{"no secrets", map[string]string{}, "PROVIDER_SECRETS is empty"},
		{"no delivery url", map[string]string{"DELIVERY_URL": "", "PROVIDER_SECRETS": "a=" + goodSecret}, "DELIVERY_URL"},
		{"delivery url without scheme", map[string]string{"DELIVERY_URL": "localhost:9000", "PROVIDER_SECRETS": "a=" + goodSecret}, "DELIVERY_URL"},
		{"delivery url ftp", map[string]string{"DELIVERY_URL": "ftp://files.example", "PROVIDER_SECRETS": "a=" + goodSecret}, "DELIVERY_URL"},
		{"short delivery secret", map[string]string{"DELIVERY_SECRET": "short", "PROVIDER_SECRETS": "a=" + goodSecret}, "DELIVERY_SECRET"},
		{"zero attempts", map[string]string{"MAX_ATTEMPTS": "0", "PROVIDER_SECRETS": "a=" + goodSecret}, "MAX_ATTEMPTS"},
		{"attempts not a number", map[string]string{"MAX_ATTEMPTS": "many", "PROVIDER_SECRETS": "a=" + goodSecret}, "MAX_ATTEMPTS"},
		{"missing equals", map[string]string{"PROVIDER_SECRETS": "acme-pay"}, "expected provider=secret"},
		{"bad provider name", map[string]string{"PROVIDER_SECRETS": "Acme_Pay=" + goodSecret}, "must be 1-32 characters"},
		{"short secret", map[string]string{"PROVIDER_SECRETS": "acme-pay=short"}, "shorter than 32"},
		{"duplicate provider", map[string]string{"PROVIDER_SECRETS": "a=" + goodSecret + ",a=" + goodSecret}, "listed twice"},
		{"bad tolerance", map[string]string{"PROVIDER_SECRETS": "a=" + goodSecret, "SIGNATURE_TOLERANCE": "soon"}, "SIGNATURE_TOLERANCE"},
		{"negative tolerance", map[string]string{"PROVIDER_SECRETS": "a=" + goodSecret, "SIGNATURE_TOLERANCE": "-1m"}, "SIGNATURE_TOLERANCE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(env(tt.vars))
			if err == nil || !strings.Contains(err.Error(), tt.wantText) {
				t.Errorf("Load() error = %v, want it to mention %q", err, tt.wantText)
			}
		})
	}
}

func TestLoadReportsEveryProblem(t *testing.T) {
	_, err := Load(env(map[string]string{"PROVIDER_SECRETS": "Bad=" + goodSecret + ",good=short"}))
	if err == nil {
		t.Fatal("Load() error = nil")
	}
	for _, want := range []string{"must be 1-32 characters", "shorter than 32"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestErrorsNeverContainSecrets(t *testing.T) {
	secret := "do-not-print-me"
	_, err := Load(env(map[string]string{"PROVIDER_SECRETS": "acme-pay=" + secret + ",broken" + secret}))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Errorf("error leaks the secret: %v", err)
	}
}
