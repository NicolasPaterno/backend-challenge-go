//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"uuid"

	pgadapter "github.com/NicolasPaterno/backend-challenge-go/internal/adapter/postgres"
	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
	"github.com/NicolasPaterno/backend-challenge-go/internal/worker/outbox"
)

// Each publisher gets its own pool, so contention here is between connections
// rather than between goroutines sharing one (§13.6).
func newOutboxStore(t *testing.T, databaseURL string) (*pgadapter.OutboxStore, *pgxpool.Pool) {
	t.Helper()

	pool, err := pgxpool.New(context.Background(), databaseURL)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return pgadapter.NewOutboxStore(pool), pool
}

func seedEvents(t *testing.T, pool *pgxpool.Pool, n int) []uuid.UUID {
	t.Helper()

	const insert = `
		INSERT INTO outbox_events (event_id, aggregate_id, event_type, payload, occurred_at)
		VALUES ($1, $2, 'WagerTransactionProcessed', $3, now())`

	ids := make([]uuid.UUID, n)
	for i := range ids {
		ids[i] = uuid.NewV7()
		payload := []byte(`{"eventId":"` + ids[i].String() + `"}`)
		if _, err := pool.Exec(context.Background(), insert, ids[i], uuid.NewV7(), payload); err != nil {
			t.Fatalf("seed outbox event: %v", err)
		}
	}
	return ids
}

func TestTwoPublishersNeverClaimTheSameRow(t *testing.T) {
	databaseURL := testsupport.PostgresMigrated(t)
	_, pool := newOutboxStore(t, databaseURL)
	seeded := seedEvents(t, pool, 60)

	var mu sync.Mutex
	seen := map[uuid.UUID]int{}

	var wg sync.WaitGroup
	for range 2 {
		store, _ := newOutboxStore(t, databaseURL)
		wg.Go(func() {
			for {
				published, err := store.Drain(context.Background(), 7, func(_ context.Context, e outbox.Event) error {
					mu.Lock()
					defer mu.Unlock()
					seen[e.EventID]++
					return nil
				})
				if err != nil {
					t.Errorf("Drain() error = %v", err)
					return
				}
				if published == 0 {
					return
				}
			}
		})
	}
	wg.Wait()

	if len(seen) != len(seeded) {
		t.Fatalf("published %d distinct events, want %d", len(seen), len(seeded))
	}
	for id, count := range seen {
		if count != 1 {
			t.Errorf("event %s published %d times, want exactly one", id, count)
		}
	}
	if pending := countPending(t, pool); pending != 0 {
		t.Errorf("%d events left unpublished, want 0", pending)
	}
}

// §11's second recovery case: the send reached the queue but the confirming
// commit never landed. The row stays claimable and comes back with the same
// eventId, because the publisher never rewrites the payload.
func TestARowLostBetweenSendAndConfirmationIsRepublishedUnchanged(t *testing.T) {
	databaseURL := testsupport.PostgresMigrated(t)
	store, pool := newOutboxStore(t, databaseURL)
	seeded := seedEvents(t, pool, 1)

	ctx, cancel := context.WithCancel(context.Background())
	var sent outbox.Event
	_, err := store.Drain(ctx, 10, func(_ context.Context, e outbox.Event) error {
		sent = e
		cancel() // the publisher dies here, after the send and before the commit
		return nil
	})
	if err == nil {
		t.Fatal("Drain() = nil, want the cancelled commit to fail")
	}
	if pending := countPending(t, pool); pending != 1 {
		t.Fatalf("%d events pending after the lost commit, want 1", pending)
	}

	var republished outbox.Event
	if _, err := store.Drain(context.Background(), 10, func(_ context.Context, e outbox.Event) error {
		republished = e
		return nil
	}); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}

	if republished.EventID != seeded[0] || republished.EventID != sent.EventID {
		t.Errorf("republished eventId = %s, want the original %s", republished.EventID, seeded[0])
	}
	if string(republished.Payload) != string(sent.Payload) {
		t.Errorf("republished payload = %s, want the original %s", republished.Payload, sent.Payload)
	}
}

func TestAFailedSendIsBackedOffAndRetriedLater(t *testing.T) {
	databaseURL := testsupport.PostgresMigrated(t)
	store, pool := newOutboxStore(t, databaseURL)
	seedEvents(t, pool, 1)

	refused := errors.New("queue unavailable")
	published, err := store.Drain(context.Background(), 10, func(context.Context, outbox.Event) error {
		return refused
	})
	if published != 0 || !errors.Is(err, refused) {
		t.Fatalf("Drain() = (%d, %v), want (0, the send error)", published, err)
	}

	var attempts int
	var next time.Time
	const state = `SELECT attempts, next_attempt_at FROM outbox_events LIMIT 1`
	if err := pool.QueryRow(context.Background(), state).Scan(&attempts, &next); err != nil {
		t.Fatalf("read outbox state: %v", err)
	}
	if attempts != 1 {
		t.Errorf("attempts = %d, want 1", attempts)
	}
	if !next.After(time.Now()) {
		t.Errorf("next_attempt_at = %v, want a future time", next)
	}

	// The backoff has to hold the row back, or a dead queue becomes a spin.
	calls := 0
	if _, err := store.Drain(context.Background(), 10, func(context.Context, outbox.Event) error {
		calls++
		return nil
	}); err != nil {
		t.Fatalf("Drain() error = %v", err)
	}
	if calls != 0 {
		t.Errorf("claimed %d rows before the backoff expired, want 0", calls)
	}
}

func countPending(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()

	var pending int
	const query = `SELECT count(*) FROM outbox_events WHERE published_at IS NULL`
	if err := pool.QueryRow(context.Background(), query).Scan(&pending); err != nil {
		t.Fatalf("count pending events: %v", err)
	}
	return pending
}
