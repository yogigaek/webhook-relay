// Package signature signs and verifies webhook payloads with HMAC-SHA256.
//
// The header carries a timestamp and a signature, "t=<unix seconds>,v1=<hex>", and the signed
// message is "<timestamp>.<raw body>". Binding the timestamp into the signature lets the receiver
// reject a captured request that is replayed later, not only a forged one.
package signature

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"time"
)

// Header is the request header that carries the signature.
const Header = "X-Signature"

var (
	ErrMissing   = errors.New("signature header missing")
	ErrMalformed = errors.New("signature header malformed")
	ErrExpired   = errors.New("signature timestamp outside tolerance")
	ErrMismatch  = errors.New("signature does not match")
)

// Sign returns the header value for body, signed with secret at time ts.
func Sign(secret []byte, ts time.Time, body []byte) string {
	t := strconv.FormatInt(ts.Unix(), 10)
	return "t=" + t + ",v1=" + hex.EncodeToString(mac(secret, t, body))
}

// Verify checks header against body. A timestamp further than tolerance from now, in either
// direction, is rejected so a replayed request stops working after a few minutes.
func Verify(secret []byte, header string, body []byte, now time.Time, tolerance time.Duration) error {
	if header == "" {
		return ErrMissing
	}

	var t, v1 string
	for _, part := range strings.Split(header, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return ErrMalformed
		}
		switch key {
		case "t":
			t = value
		case "v1":
			v1 = value
		}
	}
	if t == "" || v1 == "" {
		return ErrMalformed
	}

	unix, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return ErrMalformed
	}
	if age := now.Sub(time.Unix(unix, 0)); age > tolerance || age < -tolerance {
		return ErrExpired
	}

	got, err := hex.DecodeString(v1)
	if err != nil {
		return ErrMalformed
	}
	// hmac.Equal takes the same time however many bytes match, so response timing reveals
	// nothing about how close a guessed signature is.
	if !hmac.Equal(got, mac(secret, t, body)) {
		return ErrMismatch
	}
	return nil
}

func mac(secret []byte, t string, body []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte(t))
	h.Write([]byte("."))
	h.Write(body)
	return h.Sum(nil)
}
