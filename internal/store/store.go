// Package store keeps received webhook events in PostgreSQL.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Event is one webhook as received, before delivery.
type Event struct {
	Provider string
	EventID  string
	Payload  []byte
}

type Store struct {
	pool *pgxpool.Pool
}

func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Save stores e and reports whether it was new. A second Save for the same provider and event id
// stores nothing and returns false: providers retry webhooks they already sent, and each event
// must be relayed once.
func (s *Store) Save(ctx context.Context, e Event) (bool, error) {
	// ON CONFLICT DO NOTHING makes the check and the insert one atomic statement. Checking first
	// and inserting after would let two copies of a retried webhook, arriving together, both pass.
	var id int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO events (provider, event_id, payload)
		VALUES ($1, $2, $3)
		ON CONFLICT (provider, event_id) DO NOTHING
		RETURNING id`,
		e.Provider, e.EventID, e.Payload,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
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
	Attempt int
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
		RETURNING e.id, e.provider, e.event_id, e.payload, e.attempts`,
		limit, lease.Seconds(),
	)
	if err != nil {
		return nil, fmt.Errorf("claim due events: %w", err)
	}
	deliveries, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Delivery, error) {
		var d Delivery
		err := row.Scan(&d.ID, &d.Provider, &d.EventID, &d.Payload, &d.Attempt)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("claim due events: %w", err)
	}
	return deliveries, nil
}

// MarkDelivered records a successful delivery; the event is never sent again.
func (s *Store) MarkDelivered(ctx context.Context, id int64) error {
	return s.exec(ctx, `
		UPDATE events SET status = 'delivered', delivered_at = now(), last_error = NULL
		WHERE id = $1`, id)
}

// Retry schedules another attempt after delay.
func (s *Store) Retry(ctx context.Context, id int64, delay time.Duration, reason string) error {
	return s.exec(ctx, `
		UPDATE events SET next_attempt_at = now() + make_interval(secs => $2), last_error = $3
		WHERE id = $1`, id, delay.Seconds(), reason)
}

// Bury moves an event to the dead-letter state: no more attempts until someone requeues it.
func (s *Store) Bury(ctx context.Context, id int64, reason string) error {
	return s.exec(ctx, `UPDATE events SET status = 'dead', last_error = $2 WHERE id = $1`, id, reason)
}

func (s *Store) exec(ctx context.Context, sql string, args ...any) error {
	if _, err := s.pool.Exec(ctx, sql, args...); err != nil {
		return fmt.Errorf("update event %v: %w", args[0], err)
	}
	return nil
}
