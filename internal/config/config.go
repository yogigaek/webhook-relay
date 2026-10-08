// Package config reads the relay's settings from environment variables.
package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// MinSecretLength rejects secrets short enough to brute-force. 32 bytes matches the key size of
// HMAC-SHA256.
const MinSecretLength = 32

// providerPattern keeps provider names safe to put in a URL, a log line and a database column.
var providerPattern = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

type Config struct {
	Addr        string
	DatabaseURL string
	// Secrets maps a provider name to its signing secret. A provider not listed here is unknown.
	Secrets            map[string][]byte
	SignatureTolerance time.Duration
}

// Load builds a Config from getenv (os.Getenv in production, a map in tests). It fails on start
// rather than at the first request; every problem in PROVIDER_SECRETS is reported at once.
func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		Addr:               ":" + or(getenv("PORT"), "8080"),
		DatabaseURL:        getenv("DATABASE_URL"),
		SignatureTolerance: 5 * time.Minute,
	}
	if cfg.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is empty: set it to a PostgreSQL connection string")
	}

	if v := getenv("SIGNATURE_TOLERANCE"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return Config{}, fmt.Errorf("SIGNATURE_TOLERANCE: %q is not a positive duration such as 5m", v)
		}
		cfg.SignatureTolerance = d
	}

	secrets, err := parseSecrets(getenv("PROVIDER_SECRETS"))
	if err != nil {
		return Config{}, err
	}
	cfg.Secrets = secrets
	return cfg, nil
}

// parseSecrets reads "provider=secret,provider2=secret2". Secrets cannot contain a comma.
func parseSecrets(raw string) (map[string][]byte, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, errors.New("PROVIDER_SECRETS is empty: set it to provider=secret pairs separated by commas")
	}

	secrets := map[string][]byte{}
	var errs []error
	for i, pair := range strings.Split(raw, ",") {
		name, secret, ok := strings.Cut(strings.TrimSpace(pair), "=")
		switch {
		case !ok:
			// the pair itself is not echoed back: it may hold a secret
			errs = append(errs, fmt.Errorf("PROVIDER_SECRETS entry %d: expected provider=secret", i+1))
		case !providerPattern.MatchString(name):
			errs = append(errs, fmt.Errorf("PROVIDER_SECRETS entry %d: provider %q must be 1-32 characters of a-z, 0-9 or -", i+1, name))
		case len(secret) < MinSecretLength:
			errs = append(errs, fmt.Errorf("PROVIDER_SECRETS: secret for %q is shorter than %d characters", name, MinSecretLength))
		case secrets[name] != nil:
			errs = append(errs, fmt.Errorf("PROVIDER_SECRETS: provider %q is listed twice", name))
		default:
			secrets[name] = []byte(secret)
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return secrets, nil
}

func or(v, fallback string) string {
	if v != "" {
		return v
	}
	return fallback
}
