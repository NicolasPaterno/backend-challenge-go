-- the retry state for a reversal waiting on its reference: how many times it
-- has been looked for, when to look again, and when to give up.
--
-- The deadline is stamped when the wait is recorded, from REFERENCE_TTL, so a
-- row carries the prazo it was accepted under and a later change of policy does
-- not retroactively expire it. NULL on every row that is not waiting.
--
-- The two defaults spare the insert path columns it only ever sets to those
-- values; on a row that is not waiting they are read by nothing, which the
-- partial index below makes literal.
ALTER TABLE wager_transactions
    ADD COLUMN attempts              INTEGER     NOT NULL DEFAULT 0,
    ADD COLUMN next_attempt_at       TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN reference_deadline_at TIMESTAMPTZ;

CREATE INDEX wager_transactions_pending_reference
    ON wager_transactions (next_attempt_at, id)
    WHERE status = 'PENDING_REFERENCE';
