// Package store keeps received webhook events in PostgreSQL.
package store

import (
	"context"
	"errors"
	"fmt"

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
