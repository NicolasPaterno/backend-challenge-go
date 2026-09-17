DROP INDEX wager_transactions_pending_reference;

ALTER TABLE wager_transactions
    DROP COLUMN reference_deadline_at,
    DROP COLUMN next_attempt_at,
    DROP COLUMN attempts;
