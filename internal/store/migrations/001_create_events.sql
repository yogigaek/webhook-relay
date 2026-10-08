CREATE TABLE events (
    id              BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    provider        TEXT        NOT NULL,
    -- the provider's own event id; with provider it is the idempotency key
    event_id        TEXT        NOT NULL,
    payload         JSONB       NOT NULL,
    received_at     TIMESTAMPTZ NOT NULL DEFAULT now(),

    -- delivery state, used by the relay worker in stage 4
    status          TEXT        NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending', 'delivered', 'dead')),
    attempts        INT         NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_error      TEXT,
    delivered_at    TIMESTAMPTZ,

    -- a provider that retries a webhook it already sent must not create a second row
    CONSTRAINT events_provider_event_id_key UNIQUE (provider, event_id)
);

-- the worker only ever looks for pending events that are due, so the index covers just those rows
CREATE INDEX events_due_idx ON events (next_attempt_at) WHERE status = 'pending';
