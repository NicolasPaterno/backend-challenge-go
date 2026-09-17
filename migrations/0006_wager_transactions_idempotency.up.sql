-- §9: one record per idempotency key within a provider, and one financial
-- operation per (provider_id, external_transaction_id) whatever key it arrived
-- under. An INTERNAL row leaves both columns NULL and NULLs never conflict, so
-- neither index constrains an OPENING.
CREATE UNIQUE INDEX wager_transactions_provider_key_unique
    ON wager_transactions (provider_id, idempotency_key);

CREATE UNIQUE INDEX wager_transactions_provider_external_unique
    ON wager_transactions (provider_id, external_transaction_id);
