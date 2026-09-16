CREATE TABLE wallets (
    id            UUID        PRIMARY KEY,
    player_id     UUID        NOT NULL,
    currency      TEXT        NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance_minor BIGINT      NOT NULL CHECK (balance_minor >= 0), -- §5.8
    version       BIGINT      NOT NULL CHECK (version >= 1),
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL,
    CONSTRAINT wallets_player_currency_unique UNIQUE (player_id, currency)
);
