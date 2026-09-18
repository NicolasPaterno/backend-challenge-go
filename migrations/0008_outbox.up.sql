-- the event is written in the commit that caused it and published later,
-- so nothing can reach a consumer before the money moved.
CREATE TABLE outbox_events (
    event_id     UUID NOT NULL PRIMARY KEY,
    -- The brief makes the wallet the root of the financial aggregate, so every event
    -- here belongs to one. 14 groups the queue by it.
    aggregate_id UUID NOT NULL,
    event_type   TEXT NOT NULL,
    payload      JSONB NOT NULL,
    occurred_at  TIMESTAMPTZ NOT NULL,

    attempts        INT NOT NULL DEFAULT 0,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    published_at    TIMESTAMPTZ
);

-- The publisher of 11 reads nothing else: the oldest unpublished row that is due.
CREATE INDEX outbox_events_pending
    ON outbox_events (next_attempt_at, event_id)
    WHERE published_at IS NULL;
