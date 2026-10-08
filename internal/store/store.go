// Package store keeps received webhook events in PostgreSQL.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is one webhook as received, before delivery.
type Event struct {
	Provider string
	EventID  string
	Payload  []byte
	// TraceParent is the W3C traceparent of the receiving request, or "" when there is none.
	TraceParent string
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// ErrInvalidPayload means PostgreSQL refused the payload itself, so retrying cannot help. The one
// known case: JSON that is valid but holds \u0000, which a JSONB column cannot store.
var ErrInvalidPayload = errors.New("payload cannot be stored")

// Save stores e and reports whether it was new. A second Save for the same provider and event id
// stores nothing and returns false: providers retry webhooks they already sent, and each event
// must be relayed once.
func (s *Store) Save(ctx context.Context, e Event) (bool, error) {
	// ON CONFLICT DO NOTHING makes the check and the insert one atomic statement. Checking first
	// and inserting after would let two copies of a retried webhook, arriving together, both pass.
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO events (provider, event_id, payload, trace_parent)
		VALUES ($1, $2, $3, NULLIF($4, ''))
		ON CONFLICT (provider, event_id) DO NOTHING
		RETURNING id`,
		e.Provider, e.EventID, e.Payload, e.TraceParent,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "22P05" { // untranslatable_character
		return false, ErrInvalidPayload
	}
	if err != nil {
		return false, fmt.Errorf("save event: %w", err)
	}
	return true, nil
}

// Ping reports whether the database answers; the readiness check uses it.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// Delivery is an event claimed by the relay worker.
type Delivery struct {
	ID       int64
	Provider string
	EventID  string
	Payload  []byte
	// Attempt counts this one: 1 on the first delivery.
	Attempt     int
	TraceParent string
}

// ClaimDue takes up to limit pending events that are due and leases them for lease: they are not
// due again until the lease runs out. The worker holds no lock while it calls the destination, and
// if it crashes mid-delivery the event comes back by itself when the lease expires.
//
// FOR UPDATE SKIP LOCKED lets several workers claim at once without waiting on, or double-claiming,
// each other's rows.
func (s *Store) ClaimDue(ctx context.Context, limit int, lease time.Duration) ([]Delivery, error) {
	// The CTE pins the locked set: written as "WHERE id IN (subquery)", the planner may run the
	// subquery more than once and claim more than limit rows.
	rows, err := s.pool.Query(ctx, `
		WITH due AS (
			SELECT id FROM events
			WHERE status = 'pending' AND next_attempt_at <= now()
			ORDER BY next_attempt_at
			LIMIT $1
			FOR UPDATE SKIP LOCKED
		)
		UPDATE events e SET
			attempts = e.attempts + 1,
			next_attempt_at = now() + make_interval(secs => $2)
		FROM due
		WHERE e.id = due.id
		RETURNING e.id, e.provider, e.event_id, e.payload, e.attempts, COALESCE(e.trace_parent, '')`,
		limit, lease.Seconds(),
	)
	if err != nil {
		return nil, fmt.Errorf("claim due events: %w", err)
	}
	deliveries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Delivery, error) {
		var d Delivery
		err := row.Scan(&d.ID, &d.Provider, &d.EventID, &d.Payload, &d.Attempt, &d.TraceParent)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("claim due events: %w", err)
	}
	return deliveries, nil
}

// ErrLeaseLost means the outcome was not recorded because the event was claimed again after this
// attempt's lease ran out; the newer attempt owns the event now.
var ErrLeaseLost = errors.New("lease lost: event was claimed again")

// The outcome methods take the attempt number they are reporting on and only update the row while
// it is still on that attempt. Without this fencing, a worker that overran its lease could
// overwrite the result of the newer attempt that took the event over.

// MarkDelivered records a successful delivery; the event is never sent again.
func (s *Store) MarkDelivered(ctx context.Context, id int64, attempt int) error {
	return s.recordOutcome(ctx, `
		UPDATE events SET status = 'delivered', delivered_at = now(), last_error = NULL
		WHERE id = $1 AND attempts = $2 AND status = 'pending'`, id, attempt)
}

// Retry schedules another attempt after delay.
func (s *Store) Retry(ctx context.Context, id int64, attempt int, delay time.Duration, reason string) error {
	return s.recordOutcome(ctx, `
		UPDATE events SET next_attempt_at = now() + make_interval(secs => $3), last_error = $4
		WHERE id = $1 AND attempts = $2 AND status = 'pending'`, id, attempt, delay.Seconds(), reason)
}

// Bury moves an event to the dead-letter state: no more attempts until someone requeues it.
func (s *Store) Bury(ctx context.Context, id int64, attempt int, reason string) error {
	return s.recordOutcome(ctx, `
		UPDATE events SET status = 'dead', last_error = $3
		WHERE id = $1 AND attempts = $2 AND status = 'pending'`, id, attempt, reason)
}

func (s *Store) recordOutcome(ctx context.Context, sql string, id int64, attempt int, args ...any) error {
	tag, err := s.pool.Exec(ctx, sql, append([]any{id, attempt}, args...)...)
	if err != nil {
		return fmt.Errorf("update event %d: %w", id, err)
	}
	if tag.RowsAffected() == 0 {
		return ErrLeaseLost
	}
	return nil
}
