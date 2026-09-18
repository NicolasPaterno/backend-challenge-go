package postgres

import (
	"context"
	"encoding/json"
	"expvar"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/events"
	"github.com/NicolasPaterno/backend-challenge-go/internal/worker/outbox"
)

// attempts, next_attempt_at and published_at are left to their defaults: 11
// owns them, and nothing here has attempted a publication.
const insertOutboxEvent = `
	INSERT INTO outbox_events (event_id, aggregate_id, event_type, payload, occurred_at)
	VALUES ($1, $2, $3, $4, $5)`

// Called inside the caller's transaction, never with its own: the brief allows a
// publication only after the commit that caused it, which holds because the row
// cannot exist without that commit.
func insertOutbox(ctx context.Context, tx pgx.Tx, outbox []events.Envelope) error {
	for _, e := range outbox {
		payload, err := json.Marshal(e)
		if err != nil {
			return fmt.Errorf("marshal %s: %w", e.EventType, err)
		}
		if _, err := tx.Exec(ctx, insertOutboxEvent,
			e.EventID, e.AggregateID, e.EventType, payload, e.OccurredAt); err != nil {
			return fmt.Errorf("insert outbox %s: %w", e.EventType, err)
		}
	}
	return nil
}

type OutboxStore struct {
	pool *pgxpool.Pool
}

func NewOutboxStore(pool *pgxpool.Pool) *OutboxStore {
	return &OutboxStore{pool: pool}
}

const (
	// SKIP LOCKED is the claim: a row another publisher holds is invisible here,
	// so several of them drain the same table without ever meeting on a row.
	// The lease is the transaction itself — a publisher that dies has its
	// rows released by Postgres at once, with no lease column to expire.
	claimDueEvents = `
		SELECT event_id, aggregate_id, payload
		FROM outbox_events
		WHERE published_at IS NULL AND next_attempt_at <= now()
		ORDER BY next_attempt_at, event_id
		LIMIT $1
		FOR UPDATE SKIP LOCKED`

	markPublished = `UPDATE outbox_events SET published_at = now() WHERE event_id = ANY($1)`

	// Doubling from one second, capped at five minutes, computed with an integer
	// shift because power() is floating point.
	backOff = `
		UPDATE outbox_events
		SET attempts = attempts + 1,
		    next_attempt_at = now() + least(1 << least(attempts, 8), 300) * interval '1 second'
		WHERE event_id = ANY($1)`
)

func (s *OutboxStore) Drain(ctx context.Context, limit int, publish func(context.Context, outbox.Event) error) (int, error) {
	var published int
	var sendErr error
	err := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, claimDueEvents, limit)
		if err != nil {
			return fmt.Errorf("claim outbox events: %w", err)
		}
		claimed, err := pgx.CollectRows(rows, pgx.RowToStructByPos[outbox.Event])
		if err != nil {
			return fmt.Errorf("read claimed outbox events: %w", err)
		}

		var sent, failed []uuid.UUID
		for _, e := range claimed {
			if err := publish(ctx, e); err != nil {
				failed = append(failed, e.EventID)
				sendErr = fmt.Errorf("publish %d of %d events: %w", len(failed), len(claimed), err)
				continue
			}
			sent = append(sent, e.EventID)
		}

		if len(sent) > 0 {
			if _, err := tx.Exec(ctx, markPublished, sent); err != nil {
				return fmt.Errorf("mark outbox events published: %w", err)
			}
		}
		if len(failed) > 0 {
			if _, err := tx.Exec(ctx, backOff, failed); err != nil {
				return fmt.Errorf("back off outbox events: %w", err)
			}
		}

		published = len(sent)
		// A failed send is reported after the commit, never as a rollback: the
		// backoff is the outcome of the cycle, and losing it would spin on the
		// same row.
		return nil
	})
	if err != nil {
		return published, err
	}
	return published, sendErr
}

// the outbox lag: the age of the oldest event still waiting. Published as a
// function rather than kept up to date by the publisher, so it is measured from
// the table — the one place a stalled or dead publisher still shows up.
const selectOutboxLag = `
	SELECT COALESCE(EXTRACT(EPOCH FROM now() - MIN(occurred_at)), 0)::bigint
	FROM outbox_events WHERE published_at IS NULL`

// The pool is swapped rather than captured, because expvar.Publish panics on a
// second registration of the same name and the tests build the application many
// times in one process.
var lagPool atomic.Pointer[pgxpool.Pool]

var publishLagOnce = sync.OnceFunc(func() {
	expvar.Publish("outbox_lag_seconds", expvar.Func(func() any {
		pool := lagPool.Load()
		if pool == nil {
			return nil
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()

		var seconds int64
		if err := pool.QueryRow(ctx, selectOutboxLag).Scan(&seconds); err != nil {
			return nil
		}
		return seconds
	}))
})

func PublishOutboxLag(pool *pgxpool.Pool) {
	lagPool.Store(pool)
	publishLagOnce()
}
