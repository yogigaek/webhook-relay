package signature

import (
	"errors"
	"testing"
	"time"
)

func TestVerify(t *testing.T) {
	secret := []byte("test-secret-at-least-32-bytes-long!!")
	body := []byte(`{"id":"evt_1"}`)
	now := time.Unix(1_800_000_000, 0)
	tolerance := 5 * time.Minute
	valid := Sign(secret, now, body)

	tests := []struct {
		name    string
		secret  []byte
		header  string
		body    []byte
		wantErr error
	}{
		{"valid", secret, valid, body, nil},
		{"valid at edge of tolerance", secret, Sign(secret, now.Add(-tolerance), body), body, nil},
		{"missing header", secret, "", body, ErrMissing},
		{"no timestamp", secret, "v1=abcd", body, ErrMalformed},
		{"no signature", secret, "t=1800000000", body, ErrMalformed},
		{"part without equals", secret, "t=1800000000,v1", body, ErrMalformed},
		{"timestamp not a number", secret, "t=yesterday,v1=abcd", body, ErrMalformed},
		{"signature not hex", secret, "t=1800000000,v1=zz", body, ErrMalformed},
		{"too old", secret, Sign(secret, now.Add(-tolerance-time.Second), body), body, ErrExpired},
		{"too far in future", secret, Sign(secret, now.Add(tolerance+time.Second), body), body, ErrExpired},
		{"body changed", secret, valid, []byte(`{"id":"evt_2"}`), ErrMismatch},
		{"wrong secret", []byte("another-secret-at-least-32-bytes!!"), valid, body, ErrMismatch},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Verify(tt.secret, tt.header, tt.body, now, tolerance)
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Verify() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

// The format must match what other tools produce, so a provider (or openssl in the README) can
// sign requests without this package. The expected value was computed independently with:
//
//	printf '1800000000.{"id":"evt_1"}' | openssl dgst -sha256 -hmac 'test-secret-at-least-32-bytes-long!!'
func TestSignKnownVector(t *testing.T) {
	got := Sign([]byte("test-secret-at-least-32-bytes-long!!"), time.Unix(1_800_000_000, 0), []byte(`{"id":"evt_1"}`))
	want := "t=1800000000,v1=ce2ad47ffb9a2a9a756e2407fb677437df26e65756e0f6305b15a857693f616d"
	if got != want {
		t.Errorf("Sign() = %s, want %s", got, want)
	}
}
