CREATE TABLE wallet_ledger_entries (
    id             UUID NOT NULL PRIMARY KEY,
    wallet_id      UUID NOT NULL REFERENCES wallets (id),
    transaction_id UUID NOT NULL REFERENCES wager_transactions (id),
    direction      TEXT NOT NULL CHECK (direction IN ('DEBIT', 'CREDIT')),
    currency       TEXT NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),

    amount_minor         BIGINT NOT NULL CHECK (amount_minor > 0),
    balance_before_minor BIGINT NOT NULL CHECK (balance_before_minor >= 0),
    balance_after_minor  BIGINT NOT NULL CHECK (balance_after_minor >= 0),

    created_at TIMESTAMPTZ NOT NULL,

    -- §5.8: one entry per movement, so a redelivered operation cannot post twice.
    CONSTRAINT wallet_ledger_entries_wallet_transaction_unique UNIQUE (wallet_id, transaction_id),
    -- §6.4, restated here so no client can write a row the domain would refuse.
    CONSTRAINT wallet_ledger_entries_equation CHECK (
        balance_after_minor = balance_before_minor
            + CASE direction WHEN 'CREDIT' THEN amount_minor ELSE -amount_minor END
    )
);

-- §5.5: append-only, enforced against admin sessions and future bugs too.
CREATE FUNCTION wallet_ledger_entries_append_only() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'wallet_ledger_entries is append-only: % is refused', TG_OP
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER wallet_ledger_entries_append_only
    BEFORE UPDATE OR DELETE ON wallet_ledger_entries
    FOR EACH ROW EXECUTE FUNCTION wallet_ledger_entries_append_only();
