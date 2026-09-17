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
| `SHUTDOWN_TIMEOUT` | `15s` | Budget for draining in-flight requests on `SIGTERM` |
| `STARTUP_TIMEOUT` | `15s` | Budget for the dependency checks at boot |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | Slow-header protection |
| `OIDC_ISSUER_URL` | — | **Required.** Issuer as it appears in the tokens |
| `OIDC_DISCOVERY_URL` | the issuer | Where the metadata and JWKS are fetched from |
| `OIDC_AUDIENCE` | — | **Required.** Audience the tokens must carry |

Startup validates all of them at once and refuses to build the application if
any is invalid, so a misconfigured deployment fails immediately and reports
every problem in one message.

## Start the environment

```sh
docker compose up --build
```

This starts PostgreSQL and Keycloak, runs the migrations to completion, then
starts the API.

```sh
curl -i http://localhost:8080/health/live
# HTTP/1.1 200 OK
# {"status":"ok"}
```

Shut down with `Ctrl+C`, or `docker compose down -v` to also drop the database
volume.

## Authentication

Every wallet route requires a validated OIDC access token (§2); the health
checks stay public. Keycloak imports `keycloak/realm.json` on start, which
provisions the realm `wagering` and these `client_credentials` clients — local
secrets, all of the form `<clientId>-secret`:

| Client | Carries | Allowed on |
| --- | --- | --- |
| `internal-service` | the `wallets` scope | the wallet routes |
| `provider-a`, `provider-b` | `provider_id` | provider routes, from story 08 |
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
`403`. Both are `application/problem+json` and neither carries wallet data.

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

Integration tests run real containers — PostgreSQL and Keycloak, never mocks,
per §13 and are gated
behind a build tag so the default run stays fast. They need a running Docker:

```sh
go test -race -tags integration -timeout 20m ./...
```

The `Makefile` wraps all of these: `run`, `build`, `test`, `test-race`,
`test-integration`, `vet`, `fmt`, `migrate-up`, `migrate-down`.
