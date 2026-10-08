# webhook-relay

[![CI](https://github.com/yogigaek/webhook-relay/actions/workflows/ci.yml/badge.svg)](https://github.com/yogigaek/webhook-relay/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/yogigaek/webhook-relay)](go.mod)
[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A Go service that receives payment-provider webhooks, verifies them, stores each event exactly once,
and relays it to an internal service with retries, exponential backoff, and a dead-letter state.

Webhooks look simple and fail in quiet ways: providers resend events they already sent, forged
requests can hit a public endpoint, and the internal service that should act on an event is sometimes
down. The relay sits between the two and takes those problems off the internal service.

## Contents

- [Features](#features)
- [Architecture](#architecture)
- [Getting started](#getting-started)
- [API](#api)
- [Configuration](#configuration)
- [Signature format](#signature-format)
- [Guarantees and trade-offs](#guarantees-and-trade-offs)
- [Observability](#observability)
- [Testing](#testing)
- [Operations](#operations)
- [Project structure](#project-structure)
- [Roadmap](#roadmap)
- [License](#license)

## Features

- **Signature verification**: HMAC-SHA256 over the timestamp and raw body, constant-time comparison,
  and a replay window.
- **Idempotent storage**: each `(provider, event id)` is stored once, even when copies arrive at the
  same moment.
- **Reliable delivery**: retries with exponential backoff and jitter, and a dead-letter state for
  events that cannot be delivered.
- **Safe to scale out**: several instances share the work without double-claiming events.
- **Observability**: structured JSON logs and OpenTelemetry traces that follow an event from receipt
  to delivery.
- **Documented API**: an [OpenAPI 3.1 spec](api/openapi.yaml) for the public route, the probes, and
  the delivery the internal service receives.

## Architecture

```mermaid
flowchart LR
    P[Payment provider] -- "POST /webhooks/{provider}<br/>X-Signature" --> R

    subgraph relay [webhook-relay]
        R[HTTP handler] -- "INSERT … ON CONFLICT DO NOTHING" --> DB[(PostgreSQL<br/>events)]
        W[Worker] -- "claim due events<br/>FOR UPDATE SKIP LOCKED" --> DB
    end

    W -- "POST, re-signed<br/>X-Relay-Event-Id" --> S[Internal service]
    W -. "2xx → delivered<br/>5xx / 429 / timeout → retry with backoff<br/>other 4xx or last attempt → dead" .-> DB
```

1. **Receive.** The handler checks the provider, content type and size, verifies the HMAC signature
   against the raw body, and requires a string `id` in the JSON. It stores the event and answers `202`.
   A resent event answers `200 duplicate`. A database error answers `500`, so the provider retries.
   A payload PostgreSQL can never store answers `400`, so the provider does not retry it: `\u0000`, a
   lone surrogate such as `\ud800`, or bytes that are not UTF-8 (SQLSTATE `22P05`, `22021`, `22P02`).
   Other data errors stay `500` on purpose: they can come from a bug in the relay, and a retried event
   survives it.
2. **Relay.** A worker claims due events in batches, posts each one to the internal service signed
   with its own secret, and records the outcome.

## Getting started

### Prerequisites

- Go 1.27 or later
- Docker with Compose

### Run the demo

The demo starts the relay, a sink that stands in for the internal service, PostgreSQL, Jaeger, and
Swagger UI:

```sh
docker compose --profile demo up --build
```

| Service | URL |
|---|---|
| Relay | <http://localhost:8080> |
| API docs (Swagger UI) | <http://localhost:8081> |
| Traces (Jaeger) | <http://localhost:16686> |

Send a signed webhook. The secret is the demo value from `compose.yaml`:

```sh
SECRET=demo-provider-secret-change-me-0123456789
body='{"id":"evt_1","type":"payment.succeeded","amount":150000}'
ts=$(date +%s)
sig=$(printf '%s.%s' "$ts" "$body" | openssl dgst -sha256 -hmac "$SECRET" | awk '{print $NF}')
curl -i localhost:8080/webhooks/acme-pay \
  -H 'Content-Type: application/json' -H "X-Signature: t=$ts,v1=$sig" -d "$body"
```

The relay answers `202`, the sink logs the event, and the trace appears in Jaeger. Send the same
request again to see `200 duplicate`.

### Run locally

PostgreSQL in Docker, the relay and the sink with `go run`:

```sh
docker compose up -d
export DATABASE_URL=postgres://relay:relay@localhost:5434/relay
export PROVIDER_SECRETS="acme-pay=$(openssl rand -hex 32)"
export DELIVERY_URL=http://localhost:9000/events DELIVERY_SECRET=$(openssl rand -hex 32)
go run ./cmd/sink &          # stand-in internal service on :9000, same DELIVERY_SECRET
go run ./cmd/webhook-relay
```

[`.env.example`](.env.example) lists every setting with a comment.

## API

The full contract is in [`api/openapi.yaml`](api/openapi.yaml) (OpenAPI 3.1), browsable in Swagger UI
when the demo runs.

| Method | Path | Purpose | Responses |
|---|---|---|---|
| `POST` | `/webhooks/{provider}` | Receive a provider webhook | `202` stored · `200` duplicate · `400` · `401` · `404` · `405` · `413` · `415` · `500` |
| `GET` | `/healthz` | Liveness; never touches the database | `200` |
| `GET` | `/readyz` | Readiness; pings PostgreSQL | `200` · `503` |

Errors are JSON with a fixed message, for example `{"error": "invalid signature"}`; details are only
in the relay's logs. The one exception is `405`, answered by the router in plain text with an `Allow`
header.

For the provider, `2xx` means "stored, stop sending" and `500` means "not stored, retry later". Every
other `4xx` describes a request that fails the same way if sent again.

**Delivery to the internal service.** The spec's `webhooks` section describes the request the relay
sends to `DELIVERY_URL`: the event JSON, signed with `DELIVERY_SECRET`, with the headers
`X-Relay-Provider`, `X-Relay-Event-Id`, `X-Relay-Attempt`, and `traceparent`. The internal service's
answer decides what happens next:

| Answer | Outcome |
|---|---|
| `2xx` | Delivered; never sent again |
| `408`, `429`, `5xx`, network error, timeout (10s) | Retried with backoff, up to `MAX_ATTEMPTS` |
| Any other `4xx` | Dead-letter at once |

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `DATABASE_URL` | yes | | PostgreSQL connection string |
| `PROVIDER_SECRETS` | yes | | `provider=secret` pairs, comma-separated. Names: `a-z`, `0-9`, `-`. Secrets: 32+ characters |
| `DELIVERY_URL` | yes | | http(s) URL that receives every event |
| `DELIVERY_SECRET` | yes | | Secret the relay signs deliveries with, 32+ characters |
| `MAX_ATTEMPTS` | no | `8` | Deliveries tried before an event goes to dead-letter |
| `SIGNATURE_TOLERANCE` | no | `5m` | Accepted age of a signature timestamp |
| `PORT` | no | `8080` | HTTP port |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | no | | Export traces over OTLP/HTTP, e.g. `http://localhost:4318` |

The relay checks its configuration on start and refuses to run with a missing or weak setting.
Secrets never appear in logs or error messages.

## Signature format

Inbound and outbound requests use the same header:

```
X-Signature: t=<unix seconds>,v1=<hex HMAC-SHA256 of "<t>.<raw body>">
```

The signature covers the exact bytes of the body, so compute it on the body as sent.
`internal/signature` has a test vector computed independently with `openssl`, so any language can
produce compatible signatures.

## Guarantees and trade-offs

| Property | How |
|---|---|
| An event is stored once, even when copies arrive together | `UNIQUE (provider, event_id)` and a single `INSERT … ON CONFLICT DO NOTHING`. Checking first and inserting after would let two simultaneous copies both pass. Tested with 20 concurrent copies. |
| Forged or replayed requests are rejected | HMAC-SHA256 over `"<timestamp>.<raw body>"`, compared in constant time (`hmac.Equal`), with a 5 minute window. Every signature rejection gets the same response; the reason is only logged. |
| An accepted event is never lost | It is in PostgreSQL before `202` is sent. A worker that crashes mid-delivery holds a lease, not a lock: the event becomes due again when the lease expires. The lease covers a whole batch of deliveries that each hit the delivery timeout and the 5s outcome-write timeout. |
| A slow worker cannot overwrite a newer attempt | Outcomes are written only while the row is still on the attempt being reported (`WHERE attempts = $n`), a fencing token. A worker that overran its lease finds its write ignored and logs it. |
| Several workers can run at once | `FOR UPDATE SKIP LOCKED` in a CTE: workers split the due events without waiting on or double-claiming each other's rows. Tested with 5 workers and 50 events. |
| Failures back off instead of hammering a struggling service | Exponential backoff (10s, 20s, 40s … capped at 1h), each delay jittered between half and its full value, up to `MAX_ATTEMPTS` (default 8). |

**Delivery is at-least-once, not exactly-once.** If the worker crashes after the internal service
accepted an event but before the result is written, the event is delivered again. Exactly-once across
two systems needs the receiver to deduplicate, so every delivery carries `X-Relay-Event-Id` for that.

Other design decisions:

- **Payloads are stored as `JSONB`**, so the relayed JSON is equivalent to what arrived but not
  byte-identical (whitespace and key order are normalised). The relay signs what it sends, so
  verification downstream is unaffected. Keeping the original bytes for audit would mean a `BYTEA`
  column instead.
- **A 4xx from the internal service goes straight to dead-letter** (except 408 and 429): sending the
  same bytes again gets the same answer, and a person should look at it.
- **Deliveries in a batch run one after another.** One slow destination delays the rest of its batch;
  running more instances spreads the load, which `SKIP LOCKED` already allows.

## Observability

- **Structured JSON logs** (`log/slog`). Any line written inside a request or delivery carries its
  `trace_id` and `span_id` at the top level of the line, even inside a `slog` group.
- **OpenTelemetry traces** over OTLP/HTTP when `OTEL_EXPORTER_OTLP_ENDPOINT` is set: the webhook
  request, every SQL query (`otelpgx`), each delivery attempt, and the outgoing HTTP call.
  - Delivery happens later than receipt, so it is its own trace with a **span link** back to the
    request that received the event (the receive span's `traceparent` is stored with the event).
  - The webhook route is a public endpoint: a provider's `traceparent` never decides sampling or which
    trace the receive span joins; it only becomes a span link, and its `baggage` is ignored.
  - The outgoing request sends `traceparent`, so the internal service's spans join the delivery trace.
- **Probes**: `GET /healthz` answers whether the process is alive and never touches the database;
  `GET /readyz` answers whether it can take traffic and pings PostgreSQL.

## Testing

Unit tests run anywhere. The store tests need PostgreSQL and use a separate database, so they never
touch the one a running relay uses:

```sh
go test ./...                    # store tests are skipped without a database
docker compose up -d
TEST_DATABASE_URL=postgres://relay:relay@localhost:5434/relay_test go test -race ./...
```

CI runs on pushes to `main` and on pull requests: `gofmt`, `go vet`, `staticcheck`, the full test suite with a
PostgreSQL service, a lint of the OpenAPI spec, and a Docker image build.

## Operations

Dead-lettered events stay in the table with their last error:

```sql
SELECT id, provider, event_id, attempts, last_error FROM events WHERE status = 'dead';

-- after fixing the cause, send them again:
UPDATE events SET status = 'pending', attempts = 0, next_attempt_at = now() WHERE status = 'dead';
```

On `SIGTERM` the relay stops accepting requests, lets requests in flight finish (up to 10s), stops
the worker, and flushes its traces before it exits. A delivery cut off by the shutdown records nothing:
its lease expires and the event is delivered after the restart, late but not lost.

## Project structure

```
api/openapi.yaml    OpenAPI 3.1 spec: public route, probes, delivery to the internal service
cmd/webhook-relay   entry point: config, database, migrations, HTTP server, worker, shutdown
cmd/sink            demo stand-in for the internal service
internal/config     environment variables, validated on start
internal/signature  HMAC signing and verification
internal/httpapi    routes and the webhook handler
internal/store      PostgreSQL: idempotent insert, claim, outcomes; embedded SQL migrations
internal/relay      delivery worker, backoff
internal/telemetry  OpenTelemetry setup, trace ids in logs
```

## Roadmap

- An admin endpoint to inspect and requeue dead-lettered events (today it is the SQL above).
- Per-provider event id location; every provider must currently put it in a top-level `id` field.
- Metrics (delivery latency, queue depth); only traces and logs exist today.

## License

[MIT](LICENSE) © Muhammad Yogi
