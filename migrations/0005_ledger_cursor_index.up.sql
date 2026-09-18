-- The order the cursor pages over. id is the primary key, so the triple is a
-- total order and a boundary never repeats or skips a row.
CREATE INDEX wallet_ledger_entries_cursor
    ON wallet_ledger_entries (wallet_id, created_at, id);
