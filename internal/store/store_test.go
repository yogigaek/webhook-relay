package store

import (
	"context"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests need a real PostgreSQL. They run when TEST_DATABASE_URL is set (docker compose up
// provides one) and are skipped otherwise, so `go test ./...` still works without a database.
func newTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `TRUNCATE events`); err != nil {
		t.Fatal(err)
	}
	return New(pool)
}

func TestSaveIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	e := Event{Provider: "acme-pay", EventID: "evt_1", Payload: []byte(`{"id":"evt_1"}`)}

	first, err := s.Save(ctx, e)
	if err != nil || !first {
		t.Fatalf("first Save = %v, %v; want true, nil", first, err)
	}
	second, err := s.Save(ctx, e)
	if err != nil || second {
		t.Fatalf("second Save = %v, %v; want false, nil", second, err)
	}

	// the same event id from another provider is a different event
	other, err := s.Save(ctx, Event{Provider: "other-bank", EventID: "evt_1", Payload: []byte(`{}`)})
	if err != nil || !other {
		t.Fatalf("Save for another provider = %v, %v; want true, nil", other, err)
	}
}

// Providers often fire a retry while the first attempt is still in flight. However many copies
// arrive at the same moment, exactly one may be stored.
func TestSaveConcurrentDuplicates(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	e := Event{Provider: "acme-pay", EventID: "evt_race", Payload: []byte(`{"id":"evt_race"}`)}

	const copies = 20
	var wg sync.WaitGroup
	results := make(chan bool, copies)
	for range copies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			inserted, err := s.Save(ctx, e)
			if err != nil {
				t.Error(err)
			}
			results <- inserted
		}()
	}
	wg.Wait()
	close(results)

	inserted := 0
	for ok := range results {
		if ok {
			inserted++
		}
	}
	if inserted != 1 {
		t.Errorf("%d copies inserted, want exactly 1", inserted)
	}
}

func TestMigrateTwice(t *testing.T) {
	s := newTestStore(t)
	// the second run finds every file already recorded and changes nothing
	if err := Migrate(context.Background(), s.pool); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
}
