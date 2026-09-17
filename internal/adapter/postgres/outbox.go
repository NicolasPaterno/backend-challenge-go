package postgres

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/events"
)

// attempts, next_attempt_at and published_at are left to their defaults: 11
// owns them, and nothing here has attempted a publication.
const insertOutboxEvent = `
	INSERT INTO outbox_events (event_id, aggregate_id, event_type, payload, occurred_at)
	VALUES ($1, $2, $3, $4, $5)`

// Called inside the caller's transaction, never with its own: §5.4 allows a
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
