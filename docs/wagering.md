# Submitting an operation

What story 08 built: `POST /wagering/transactions` for `kind=BET`, the two
transaction reads, and the idempotency and locking rules behind them.
`ARCHITECTURE.md` (story 18) carries the same decisions in their final form.

## Idempotency

Three identities, all in PostgreSQL — nothing is cached in a process, so a
restart changes no outcome (§5.2).

| Identity | Enforced by | Reused means |
| --- | --- | --- |
| `(provider_id, idempotency_key)` | `wager_transactions_provider_key_unique` | replay if the payload matches, `409` if it does not |
| `(provider_id, external_transaction_id)` | `wager_transactions_provider_external_unique` | `409`: the operation may not be re-applied under a second key |
| `(wallet_id, transaction_id)` | `wallet_ledger_entries_wallet_transaction_unique` | one ledger entry per operation, ever |

The `Idempotency-Key` header is mandatory and stored exactly as received. The
client may build it as `{providerId}:{externalTransactionId}`, but the server
never substitutes a computed value for one it was sent.

There is no pre-read for an existing record. Every submission attempts the
insert, and the unique violation is what says "this already happened" — the only
check that also holds against a submission racing it in another process.

### The payload hash

SHA-256 over canonical JSON, hex encoded, stored in `payload_hash`. `encoding/json`
marshals a map with its keys sorted, so the bytes depend on the values alone.

Fields, and nothing else: `providerId`, `externalTransactionId`, `playerId`,
`walletId`, `roundId`, `gameId`, `kind`, `amountMinor`, `currency`,
`referenceExternalTransactionId`.

Excluded: the idempotency key, and every transport field — HTTP headers, the SQS
envelope. That is what makes the same operation hash alike over both (§10).

Normalisation: the money is hashed as its `int64` minor units plus the currency
code, never as the received text. `"25"`, `"25.0"` and `"25.00"` are one amount
(A.3.1), so the same bet resubmitted in a different spelling under one key is a
replay, not a conflict. Everything else is hashed as received; the parser
rejects rather than normalises anything else (A.3.1).

## Concurrency

Coordination is per wallet: `SELECT … FROM wallets WHERE id = $1 FOR UPDATE`
opens the transaction that then writes the operation, the ledger entry and the
balance. Two operations on one wallet serialise; two on different wallets do
not, and no advisory or table-wide lock is taken anywhere (§5.6, §8).

The mode is `FOR NO KEY UPDATE`: the id never changes here, and the weaker lock
does not conflict with the `FOR KEY SHARE` a foreign key check takes on the row.

The balance update also carries `AND version = <the version read under the
lock>`, and a row count of zero is an error. It is redundant while the lock is
held, and it is what makes a lost update impossible if a later caller ever
reaches that statement without one (§5.7). Pessimistic lock first, optimistic
predicate behind it, which is the "justifiable combination" §8 allows.

Waiting on that lock is bounded by `DB_LOCK_TIMEOUT` (default 3s), set as a
`lock_timeout` runtime parameter on every pooled connection. A wallet contended
past it answers `503` with `Retry-After` rather than holding a pool connection
until the caller gives up. The bound is far longer than a transaction on an
uncontended wallet, so it fires on pathology, not on load.

## Status codes

| Situation | Status | Body |
| --- | --- | --- |
| Processed | `200` | `{transactionId, status, balance, idempotentReplay}` |
| Replay of a processed operation | `200` | the same, with `idempotentReplay: true` and the balance observed at the original processing |
| Still running (story 13's pending references) | `202` | the same, with no balance |
| Invalid input, missing `Idempotency-Key`, unusable `kind` | `400` | `problem+json`, `code: VALIDATION_FAILED`, every field violation at once |
| Unparsable JSON | `400` | `problem+json`, `code: MALFORMED_BODY` |
| Token does not authorize the `providerId` | `403` | `problem+json`, `code: FORBIDDEN` |
| Key reused with different content | `409` | `problem+json`, `code: IDEMPOTENCY_KEY_CONFLICT` |
| Operation resubmitted under a second key | `409` | `problem+json`, `code: EXTERNAL_TRANSACTION_CONFLICT` |
| Business rejection | `422` | `problem+json`, `code` is the `failureCode`, `instance` is the transaction's path, plus `idempotentReplay` |
| Recorded permanent failure | `500` | `problem+json`, `code` is the `failureCode` |
| Wallet contended past `DB_LOCK_TIMEOUT`, nothing applied | `503` | `problem+json`, `code: SERVICE_UNAVAILABLE`, `Retry-After` |
| Dependency unreachable, nothing applied | `503` | `problem+json`, `code: SERVICE_UNAVAILABLE`, `Retry-After` |

An unknown wallet is a rejection like any other, not a `404`: §11 owes every
rejection a `WagerTransactionRejected`, which needs a record to emit from. The
foreign key from `wager_transactions.wallet_id` to `wallets` was what prevented
that record and is dropped in `0007`; the column keeps the requested id, because
that is the point of the audit row. No financial invariant rested on the key —
money moves only through `wallet_ledger_entries`, whose own reference to
`wallets` is untouched, so a transaction naming an absent wallet can never carry
an entry.

A rejection is a persisted result, so an equivalent resubmission returns that
same `422` with `idempotentReplay: true` — §9 requires the flag on every replay,
not only on a success. It rides as an RFC 9457 extension member rather than in a
second body shape.

A `422` names the reason with a stable `failureCode` (§7) —
`INSUFFICIENT_FUNDS`, `WALLET_NOT_FOUND`, `WALLET_CURRENCY_MISMATCH` are the
ones a `BET` can reach — and its `instance` points at
`/wagering/transactions/{id}`, so the provider can read the record back.

## Authorization

The operation routes require the `wagering` scope, which only the provider
clients carry; `internal-service` does not, so the wallet surface and the
operation surface do not overlap (§2).

The acting provider comes from the token's `provider_id` claim and from nowhere
else. A body naming another provider is `403` before anything is validated. A
transaction belonging to another provider reads as `404` rather than `403`,
because a `403` would confirm the id exists.

## Example

```sh
TOKEN=$(curl -s http://localhost:8081/realms/wagering/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=provider-a \
  -d client_secret=provider-a-secret | sed -E 's/.*"access_token":"([^"]+)".*/\1/')

curl -i http://localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:transaction-123' \
  -d '{"providerId":"provider-a","externalTransactionId":"transaction-123",
       "playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
       "walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
       "roundId":"round-987","gameId":"fortune-chimp","kind":"BET",
       "money":{"amount":"25.00","currency":"BRL"}}'

curl -s http://localhost:8080/providers/provider-a/wagering/transactions/transaction-123 \
  -H "Authorization: Bearer $TOKEN"
```

Sending the first call again returns `idempotentReplay: true` and debits
nothing.
