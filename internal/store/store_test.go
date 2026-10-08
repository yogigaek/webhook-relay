package store

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// These tests need a real PostgreSQL. They run when TEST_DATABASE_URL is set (docker compose up
// provides one) and are skipped otherwise, so `go test ./...` still works without a database.
// They empty the events table, so point TEST_DATABASE_URL at a database used only for tests.
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

func saveEvents(t *testing.T, s *Store, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := s.Save(context.Background(), Event{Provider: "acme-pay", EventID: id, Payload: []byte(`{"id":"` + id + `"}`)}); err != nil {
			t.Fatal(err)
		}
	}
}

func eventState(t *testing.T, s *Store, eventID string) (status string, attempts int, lastError *string) {
	t.Helper()
	err := s.pool.QueryRow(context.Background(),
		`SELECT status, attempts, last_error FROM events WHERE event_id = $1`, eventID,
	).Scan(&status, &attempts, &lastError)
	if err != nil {
		t.Fatal(err)
	}
	return status, attempts, lastError
}

func TestClaimDueLeasesEvents(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveEvents(t, s, "evt_1", "evt_2", "evt_3")

	first, err := s.ClaimDue(ctx, 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || first[0].Attempt != 1 {
		t.Fatalf("first claim = %+v, want 2 deliveries on attempt 1", first)
	}

	// the two leased events are not due again; only the third one is left
	second, err := s.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(second) != 1 || second[0].EventID != "evt_3" {
		t.Fatalf("second claim = %+v, want only evt_3", second)
	}
}

func TestClaimDueAfterLeaseExpires(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveEvents(t, s, "evt_1")

	// a zero lease stands in for a worker that crashed and never reported back
	if _, err := s.ClaimDue(ctx, 1, 0); err != nil {
		t.Fatal(err)
	}
	again, err := s.ClaimDue(ctx, 1, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].Attempt != 2 {
		t.Fatalf("claim after expired lease = %+v, want evt_1 on attempt 2", again)
	}
}

// Workers claiming at the same moment must split the events between them, never share one.
func TestClaimDueConcurrentWorkers(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	ids := make([]string, 50)
	for i := range ids {
		ids[i] = fmt.Sprintf("evt_%d", i)
	}
	saveEvents(t, s, ids...)

	var mu sync.Mutex
	claimed := map[int64]int{}
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := s.ClaimDue(ctx, 20, time.Minute)
			if err != nil {
				t.Error(err)
			}
			mu.Lock()
			defer mu.Unlock()
			for _, d := range got {
				claimed[d.ID]++
			}
		}()
	}
	wg.Wait()

	if len(claimed) != len(ids) {
		t.Errorf("%d distinct events claimed, want %d", len(claimed), len(ids))
	}
	for id, n := range claimed {
		if n != 1 {
			t.Errorf("event %d claimed %d times", id, n)
		}
	}
}

func TestDeliveryOutcomes(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	saveEvents(t, s, "evt_ok", "evt_retry", "evt_dead")
	claimed, err := s.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	byEvent := map[string]int64{}
	for _, d := range claimed {
		byEvent[d.EventID] = d.ID
	}

	if err := s.MarkDelivered(ctx, byEvent["evt_ok"]); err != nil {
		t.Fatal(err)
	}
	if err := s.Retry(ctx, byEvent["evt_retry"], 0, "status 503"); err != nil {
		t.Fatal(err)
	}
	if err := s.Bury(ctx, byEvent["evt_dead"], "status 400"); err != nil {
		t.Fatal(err)
	}

	if status, _, lastErr := eventState(t, s, "evt_ok"); status != "delivered" || lastErr != nil {
		t.Errorf("evt_ok = %s, %v; want delivered with no error", status, lastErr)
	}
	if status, _, lastErr := eventState(t, s, "evt_retry"); status != "pending" || lastErr == nil || *lastErr != "status 503" {
		t.Errorf("evt_retry = %s, %v; want pending with the error kept", status, lastErr)
	}
	if status, _, _ := eventState(t, s, "evt_dead"); status != "dead" {
		t.Errorf("evt_dead = %s, want dead", status)
	}

	// with a zero delay the retried event is due at once; delivered and dead events never are
	again, err := s.ClaimDue(ctx, 10, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 1 || again[0].EventID != "evt_retry" || again[0].Attempt != 2 {
		t.Errorf("claim after outcomes = %+v, want only evt_retry on attempt 2", again)
	}
}
