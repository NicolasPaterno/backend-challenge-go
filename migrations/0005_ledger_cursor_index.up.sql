-- The total order the cursor pages over: unique, so a boundary never repeats
-- or skips a row (§9).
CREATE INDEX wallet_ledger_entries_cursor
    ON wallet_ledger_entries (wallet_id, created_at, id);
