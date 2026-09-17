# Running the service

Working notes for the current state of the repository. The delivery `README.md`
required by the brief (§15) is written in story 18; until then this file is the
reproduction guide, and `README.md` is still the original challenge brief.

## Prerequisites

- Docker with Compose v2 (`docker compose version`)
- Go 1.27 (only to run the tests and the tooling outside containers)

## Configuration

Every setting comes from the environment. `.env.example` lists all of them with
local values; Compose reads `.env` automatically.

```sh
cp .env.example .env
```

| Variable | Default | Purpose |
| --- | --- | --- |
| `APP_ENV` | `local` | Deployment name, attached to every log line |
| `HTTP_ADDR` | `:8080` | Listen address of the public API |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` or `error` |
| `DATABASE_URL` | — | **Required.** PostgreSQL connection string |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `10` / `1` | Connection pool bounds |
| `DB_LOCK_TIMEOUT` | `3s` | How long a statement waits for a contended row before failing |
| `SHUTDOWN_TIMEOUT` | `15s` | Budget for the whole shutdown on `SIGTERM` |
| `WORKER_DRAIN_TIMEOUT` | `5s` | Each worker's share of it, so none can spend the whole budget |
| `STARTUP_TIMEOUT` | `15s` | Budget for the dependency checks at boot |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | Slow-header protection |
| `OIDC_ISSUER_URL` | — | **Required.** Issuer as it appears in the tokens |
| `OIDC_DISCOVERY_URL` | the issuer | Where the metadata and JWKS are fetched from |
| `OIDC_AUDIENCE` | — | **Required.** Audience the tokens must carry |
| `SQS_WAGER_TRANSACTIONS_QUEUE_URL` | — | **Required.** Queue the wager consumer reads from |
| `SQS_WAGER_TRANSACTIONS_DLQ_URL` | — | **Required.** Dead-letter queue for messages that can never be handled |
| `SQS_EVENTS_QUEUE_URL` | — | **Required.** Queue the outbox publisher sends to |
| `OUTBOX_POLL_INTERVAL` | `1s` | Wait between publish cycles |
| `OUTBOX_PUBLISH_WINDOW` | `10s` | Deadline on one publish cycle |
| `OUTBOX_BATCH_SIZE` | `100` | Rows claimed per cycle |
| `REFERENCE_POLL_INTERVAL` | `1s` | Wait between reference resolution cycles |
| `REFERENCE_BATCH_SIZE` | `100` | Waiting reversals swept per cycle |
| `REFERENCE_TTL` | `24h` | How long a reversal waits for its reference before being rejected |
| `AWS_REGION` | `us-east-1` | Region the SQS client signs for |
| `SQS_ENDPOINT` | unset | LocalStack's address; unset means real SQS |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | — | Broker credentials; `test`/`test` against LocalStack |

Startup validates all of them at once and refuses to build the application if
any is invalid, so a misconfigured deployment fails immediately and reports
every problem in one message.

## Start the environment

```sh
docker compose up --build
```

This starts PostgreSQL, Keycloak and LocalStack — which creates
`wager-events.fifo`, `wager-transactions.fifo` and its dead-letter queue from
`localstack/init-queues.sh` — runs the migrations to completion, then starts the
API.

```sh
curl -i http://localhost:8080/health/live
curl -i http://localhost:8080/health/ready
curl -s http://localhost:8080/metrics
# HTTP/1.1 200 OK
# {"status":"ok"}
```

Shut down with `Ctrl+C`, or `docker compose down -v` to also drop the database
volume.

## Authentication

Every wallet and operation route requires a validated OIDC access token (§2);
the health checks stay public. Keycloak imports `keycloak/realm.json` on start, which
provisions the realm `wagering` and these `client_credentials` clients — local
secrets, all of the form `<clientId>-secret`:

| Client | Carries | Allowed on |
| --- | --- | --- |
| `internal-service` | the `wallets` scope | the wallet routes |
| `provider-a`, `provider-b` | the `wagering` scope and `provider_id` | the operation routes, each only its own transactions |
| `provider-expiring` | a one-second token | nothing; it exists for the expiry test |
| `outsider` | another audience | nothing; it exists for the audience test |

`wagering-api` is the resource server: it never requests a token and only names
the audience the API accepts.

Keycloak is published on `8081` and `KC_HOSTNAME` fixes the issuer to
`http://localhost:8081/realms/wagering`, so a token fetched from the host is the
same token the `api` container verifies. The container reaches the JWKS over the
Compose network instead, which is what `OIDC_DISCOVERY_URL` is for.

```sh
TOKEN=$(curl -s http://localhost:8081/realms/wagering/protocol/openid-connect/token \
  -d grant_type=client_credentials \
  -d client_id=internal-service \
  -d client_secret=internal-service-secret | sed -E 's/.*"access_token":"([^"]+)".*/\1/')

curl -i http://localhost:8080/wallets \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}'
```

A missing, malformed, expired or wrong-audience token gets `401` with a
`WWW-Authenticate: Bearer` header; a valid provider token on a wallet route gets
`403`, as does the internal token on an operation route. All are
`application/problem+json` and none carries wallet data.

`docs/consumer.md` covers the inbound queue: the inbox, the deletion rules, the
redrive policy and the group keys. `docs/outbox.md` covers the publisher: the claim, the backoff, and the contract
of the outbound queue. `docs/wagering.md` covers submitting an operation: the idempotency rules, the
payload hash, the per-wallet locking, and the full status-code table.

## Migrations

Migrations live in `migrations/` as `NNNN_name.up.sql` / `NNNN_name.down.sql`
and are embedded in the `migrate` binary, so it needs no files at runtime.

Against the Compose database from the host:

```sh
export DATABASE_URL='postgres://wagering:wagering@localhost:5432/wagering?sslmode=disable'

go run ./cmd/migrate up        # apply everything pending
go run ./cmd/migrate down      # roll everything back
go run ./cmd/migrate steps -1  # roll back exactly one migration
go run ./cmd/migrate version   # current version, and whether it is dirty
```

Inside the running environment:

```sh
docker compose run --rm migrate down
```

A failed migration leaves the schema *dirty*; `version` reports it, and it must
be resolved by hand before further migrations run.

## Tests

```sh
go test ./...        # unit tests, no containers
go test -race ./...  # the same under the race detector
go vet ./...
gofmt -l .           # prints nothing when formatting is clean
```

Integration tests run real containers — PostgreSQL, Keycloak and LocalStack,
never mocks, per §13 and are gated
behind a build tag so the default run stays fast. They need a running Docker:

```sh
go test -race -tags integration -timeout 20m ./...
```

### Multiple instances

`test/system` is the multi-instance suite: it builds the API with the race
detector and runs **three independent processes** against shared containers, so
the guarantees §8 and §13.4 ask for are demonstrated across processes rather
than across goroutines. It is separate from the integration run because it is
slower:

```sh
go test -race -tags 'integration,system' -timeout 40m ./test/system/...
```

Each scenario ends by reconciling every wallet it touched against its ledger.
The failure simulations live there too: an instance killed with `SIGKILL` while
the queue is being worked, a reversal whose reference has not arrived yet losing
the instance that accepted it, and three publishers draining one outbox with one
of them dying mid-flight.

To run several instances by hand instead, Compose publishes a port range:

```sh
docker compose up --build --scale api=3
curl -i http://localhost:8080/health/ready
curl -i http://localhost:8081/health/ready
curl -i http://localhost:8082/health/ready
```

The `Makefile` wraps all of these: `run`, `build`, `test`, `test-race`,
`test-integration`, `test-system`, `vet`, `fmt`, `migrate-up`, `migrate-down`.
