CREATE TABLE wager_transactions (
    id       UUID NOT NULL PRIMARY KEY,
    origin   TEXT NOT NULL CHECK (origin IN ('INTERNAL', 'EXTERNAL')),
    kind     TEXT NOT NULL CHECK (kind IN ('OPENING', 'BET', 'WIN', 'LOSS', 'REFUND', 'ROLLBACK')),
    status   TEXT NOT NULL CHECK (status IN ('PENDING', 'PENDING_REFERENCE', 'PROCESSED', 'REJECTED', 'FAILED')),

    wallet_id    UUID   NOT NULL REFERENCES wallets (id),
    player_id    UUID   NOT NULL,
    currency     TEXT   NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    amount_minor BIGINT NOT NULL,

    provider_id                       TEXT,
    external_transaction_id           TEXT,
    idempotency_key                   TEXT,
    payload_hash                      TEXT,
    round_id                          TEXT,
    game_id                           TEXT,
    reference_external_transaction_id TEXT,
    reference_transaction_id          UUID REFERENCES wager_transactions (id),

    failure_code         TEXT,
    result_balance_minor BIGINT, -- in the currency column above

    created_at TIMESTAMPTZ NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL,

    -- A.1 requirement 3: the origin decides which columns apply.
    CONSTRAINT wager_transactions_origin_fields CHECK (
        (origin = 'INTERNAL'
            AND kind = 'OPENING'
            AND provider_id IS NULL
            AND external_transaction_id IS NULL
            AND idempotency_key IS NULL
            AND payload_hash IS NULL
            AND round_id IS NULL
            AND game_id IS NULL
            AND reference_external_transaction_id IS NULL)
        OR
        (origin = 'EXTERNAL'
            AND kind <> 'OPENING'
            AND provider_id IS NOT NULL
            AND external_transaction_id IS NOT NULL
            AND idempotency_key IS NOT NULL
            AND payload_hash IS NOT NULL
            AND round_id IS NOT NULL
            AND game_id IS NOT NULL)
    )
);

-- A.1: one initial credit per wallet, whatever UUID it was assigned.
CREATE UNIQUE INDEX wager_transactions_opening_unique
    ON wager_transactions (wallet_id)
    WHERE kind = 'OPENING';
