-- TRUNCATE fires no row trigger, so 0004's guard alone lets the ledger be wiped.
CREATE TRIGGER wallet_ledger_entries_no_truncate
    BEFORE TRUNCATE ON wallet_ledger_entries
    FOR EACH STATEMENT EXECUTE FUNCTION wallet_ledger_entries_append_only();
