# 📡 webhook-relay API guide

A practical guide to the relay's HTTP API, for two readers:

- **The payment provider** (or whoever sends webhooks to the relay): [Sending a webhook](#-sending-a-webhook).
- **The internal service** that receives relayed events: [Receiving deliveries](#-receiving-deliveries).

The formal contract is the OpenAPI 3.1 spec in [`api/openapi.yaml`](../api/openapi.yaml). It wins if this guide
and the spec ever disagree. Ways to browse it:

- **Online:** [open the spec in Redoc](https://redocly.github.io/redoc/?url=https://raw.githubusercontent.com/yogigaek/webhook-relay/main/api/openapi.yaml).
- **Locally:** `make demo`, then Swagger UI at <http://localhost:8081>.

Every response shown below was recorded from a running relay.

---

## 📑 Contents

- [Endpoints at a glance](#️-endpoints-at-a-glance)
- [Signatures](#-signatures)
- [Sending a webhook](#-sending-a-webhook)
- [Responses](#-responses)
- [Receiving deliveries](#-receiving-deliveries)
- [Retries and dead-letter](#-retries-and-dead-letter)
- [Probes](#-probes)
- [Checklists](#-checklists)

---

## 🗺️ Endpoints at a glance

| Who calls it | Method | Path | Purpose |
| --- | --- | --- | --- |
| Payment provider | `POST` | `/webhooks/{provider}` | Send an event to the relay |
| Platform | `GET` | `/healthz` | Liveness: is the process alive? Never touches the database |
| Platform | `GET` | `/readyz` | Readiness: can it take traffic? Pings PostgreSQL |
| The relay | `POST` | your `DELIVERY_URL` | Delivers each stored event to your internal service |

`{provider}` is a name from `PROVIDER_SECRETS` (`a-z`, `0-9`, `-`), for example `acme-pay`.

---

## 🔏 Signatures

Requests in **both directions** carry the same header:

```
X-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<raw body>">
```

| Direction | Signed with |
| --- | --- |
| Provider → relay | that provider's secret in `PROVIDER_SECRETS` |
| Relay → internal service | `DELIVERY_SECRET` |

Three rules:

1. **Sign the exact bytes.** The HMAC covers the timestamp, a dot, and the body exactly as sent. Compute it on
   the raw body, before any JSON parsing or re-encoding.
2. **Fresh only.** A timestamp more than `SIGNATURE_TOLERANCE` (default 5 minutes) away from the receiver's
   clock, in either direction, is rejected. This stops replays of old requests.
3. **Compare in constant time** (`hmac.Equal` in Go, `crypto.timingSafeEqual` in Node.js), never with `==`.

**Test vector** (the same one the repository's tests check, computed independently with `openssl`):

| Input | Value |
| --- | --- |
| Secret | `test-secret-at-least-32-bytes-long!!` |
| Timestamp | `1800000000` |
| Body | `{"id":"evt_1"}` |
| Header | `t=1800000000,v1=ce2ad47ffb9a2a9a756e2407fb677437df26e65756e0f6305b15a857693f616d` |

If your code produces that header from those inputs, it signs and verifies the same way the relay does.

---

## 📤 Sending a webhook

```
POST /webhooks/{provider}
Content-Type: application/json
X-Signature: t=…,v1=…
```

**Body:** any JSON object with a string `id` (1 to 255 bytes), at most 1 MiB in total. Every other field is
yours and is relayed as it is.

```json
{ "id": "evt_1", "type": "payment.succeeded", "amount": 150000, "currency": "IDR" }
```

**`id` is the idempotency key.** The provider in the path plus the `id` identify an event: sending it again
stores nothing and answers `200 duplicate`, so retries are always safe.

**Try it** against the demo (`make demo`; the secret is the demo value from `compose.yaml`):

```sh
SECRET=demo-provider-secret-change-me-0123456789
body='{"id":"evt_1","type":"payment.succeeded","amount":150000}'
ts=$(date +%s)
sig=$(printf '%s.%s' "$ts" "$body" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $NF}')

curl -i localhost:8080/webhooks/acme-pay \
  -H 'Content-Type: application/json' \
  -H "X-Signature: t=$ts,v1=$sig" \
  -d "$body"
```

---

## 🚦 Responses

The checks run in this order, and the first one that fails answers:

1. provider is configured → else `404`
2. `Content-Type` is `application/json` (a `charset` parameter is fine) → else `415`
3. body is at most 1 MiB → else `413`
4. signature is valid and fresh → else `401`
5. body is a JSON object with a string `id` of 1 to 255 bytes → else `400`
6. the event is stored → `202`, or `200` if it was stored before

| Status | Body | Meaning | Should the provider retry? |
| --- | --- | --- | --- |
| `202` | `{"status":"accepted"}` | Stored for the first time; it will be relayed | No |
| `200` | `{"status":"duplicate"}` | Stored before; nothing changed | No |
| `400` | `{"error":"body must be a JSON object with a string \"id\""}` | Not an object with a valid `id` | No, fix the payload |
| `400` | `{"error":"payload cannot be stored"}` | Holds data PostgreSQL can never store (`\u0000`, a lone surrogate such as `\ud800`, or bytes that are not UTF-8) | No |
| `400` | `{"error":"could not read body"}` | The body could not be read | No |
| `401` | `{"error":"invalid signature"}` | Missing, malformed, stale or wrong signature. Same answer in every case; the reason is only logged | No, fix the signature |
| `404` | `{"error":"unknown provider"}` | The provider is not in `PROVIDER_SECRETS` | No |
| `405` | `Method Not Allowed` (plain text, `Allow: POST`) | Only `POST` is accepted | No |
| `413` | `{"error":"payload too large"}` | Body over 1 MiB | No |
| `415` | `{"error":"content type must be application/json"}` | Missing or wrong `Content-Type` | No |
| `500` | `{"error":"could not store event"}` | Not stored (for example, the database is down) | **Yes, later** |

In short: **`2xx` = stored, stop sending · `500` = not stored, retry later · any other `4xx` = the same request
will fail the same way.**

Error messages are fixed on purpose. Details, such as why a signature was rejected, are only in the relay's logs,
so the public endpoint gives an attacker nothing to learn from.

---

## 📥 Receiving deliveries

For every stored event, the relay sends this to your `DELIVERY_URL`:

```
POST {DELIVERY_URL}
Content-Type: application/json
X-Signature: t=…,v1=…              ← signed with DELIVERY_SECRET
X-Relay-Provider: acme-pay         ← where the event came from
X-Relay-Event-Id: evt_1            ← the provider's event id
X-Relay-Attempt: 1                 ← 1 on the first try, then 2, 3, …
traceparent: 00-…                  ← W3C trace context, so your spans join the delivery trace

{"id": "evt_1", "type": "payment.succeeded", "amount": 150000}
```

**The body is equivalent to what the provider sent, not byte-identical.** Events are stored as `JSONB`, which
normalises whitespace and key order. The relay signs the bytes it sends, so always verify against the raw body
you receive.

**Delivery is at-least-once.** If the relay restarts after your service accepted an event but before it
recorded the result, you get the event again. Deduplicate on `X-Relay-Provider` + `X-Relay-Event-Id`.

### Verify the signature

The relay's own `internal/signature` package cannot be imported from another module, so here is the same check
in a few lines. Both versions are tested against the [test vector](#-signatures) above.

<details>
<summary><b>Go</b></summary>

```go
import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// verifyRelaySignature checks X-Signature on a request from the relay. body is the raw request body.
func verifyRelaySignature(secret []byte, header string, body []byte, now time.Time, tolerance time.Duration) bool {
	var t, v1 string
	for _, part := range strings.Split(header, ",") {
		key, value, _ := strings.Cut(strings.TrimSpace(part), "=")
		switch key {
		case "t":
			t = value
		case "v1":
			v1 = value
		}
	}
	unix, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return false
	}
	if age := now.Sub(time.Unix(unix, 0)); age > tolerance || age < -tolerance {
		return false
	}
	got, err := hex.DecodeString(v1)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(t + "."))
	mac.Write(body)
	return hmac.Equal(got, mac.Sum(nil))
}
```

A complete receiver that does this is [`cmd/sink`](../cmd/sink/main.go), the demo's stand-in internal service.

</details>

<details>
<summary><b>Node.js</b></summary>

```js
import { createHmac, timingSafeEqual } from 'node:crypto';

/** Checks X-Signature on a request from the relay. `rawBody` is the body exactly as received (a Buffer). */
export function verifyRelaySignature(secret, header, rawBody, { toleranceSeconds = 300, now = Date.now() } = {}) {
  const fields = new Map(
    (header ?? '').split(',').map((part) => {
      const [key, ...rest] = part.trim().split('=');
      return [key, rest.join('=')];
    }),
  );
  const t = fields.get('t');
  const v1 = fields.get('v1');
  if (!t || !v1 || !/^\d+$/.test(t) || !/^[0-9a-f]+$/i.test(v1)) return false;
  if (Math.abs(now / 1000 - Number(t)) > toleranceSeconds) return false;

  const expected = createHmac('sha256', secret).update(`${t}.`).update(rawBody).digest();
  const given = Buffer.from(v1, 'hex');
  return given.length === expected.length && timingSafeEqual(given, expected);
}
```

With Express, read the raw body (`express.raw({ type: 'application/json' })`) rather than the parsed JSON, or
the bytes you verify will not be the bytes that were signed.

</details>

### What your answer means

| You answer | The relay |
| --- | --- |
| `2xx` | Marks the event delivered; it is never sent again |
| `408`, `429`, any `5xx`, a network error, or no answer within 10 seconds | Retries later with backoff |
| Any other `4xx` | Moves the event to dead-letter at once: the same bytes would get the same answer, so a person should look at it |

---

## 🔁 Retries and dead-letter

A failed delivery is tried again with exponential backoff, each delay jittered between half and its full value:

| Attempt that failed | 1 | 2 | 3 | 4 | 5 | 6 | 7 | 8 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Next try in (between half and the full value) | 10s | 20s | 40s | 80s | 160s | 320s | 640s | dead-letter |

With the default `MAX_ATTEMPTS` of **8**, the eighth failure moves the event to **dead-letter**, where it stays in
the database with its last error. A higher `MAX_ATTEMPTS` keeps doubling the delay (1280s, 2560s, …) up to a cap
of 1 hour. There is no admin endpoint for it yet; the README's [Operations](../README.md#-operations) section has the
SQL to list dead-lettered events and send them again.

---

## 🩺 Probes

| Request | Answer | Use it for |
| --- | --- | --- |
| `GET /healthz` | `200 {"status":"ok"}` | Liveness. Never touches the database, so a database outage does not get the process restarted |
| `GET /readyz` | `200 {"status":"ready"}` or `503 {"error":"database unavailable"}` | Readiness. Pings PostgreSQL with a 2 second timeout |

Both answer `405` (plain text, `Allow: GET, HEAD`) to other methods.

---

## ✅ Checklists

**Sending to the relay**

- [ ] Every event has a unique string `id`, and a resend uses the same `id`
- [ ] The signature is computed on the exact body bytes sent, with a current timestamp
- [ ] `Content-Type: application/json`, body under 1 MiB
- [ ] Retry on `500` (and on a network error); stop on any `2xx`; do not retry other `4xx`

**Receiving from the relay**

- [ ] Verify `X-Signature` against the **raw** body with `DELIVERY_SECRET`, in constant time
- [ ] Deduplicate on `X-Relay-Provider` + `X-Relay-Event-Id`
- [ ] Answer `2xx` once the event is safely handled; answer `5xx` for a temporary problem so it is retried
- [ ] Answer another `4xx` only for an event that will never succeed: it goes straight to dead-letter
- [ ] Answer within 10 seconds; do slow work after replying
