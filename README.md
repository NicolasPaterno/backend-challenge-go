# Processamento Distribuído de Apostas

Serviço em Go, composto com Uber Fx, que processa operações financeiras de provedores de jogos
(`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`) sobre carteiras de jogadores — por HTTP e por SQS,
pelo mesmo caso de uso — com idempotência persistente, coordenação por carteira, outbox
transacional e recuperação de falhas demonstrada com três processos reais.

Este arquivo é o guia de reprodução: pré-requisitos, variáveis, filas, migrations, execução,
exemplos de chamada e comandos de teste. O enunciado original está preservado em
[`DESAFIO.md`](DESAFIO.md); as decisões técnicas e seus trade-offs, em
[`ARCHITECTURE.md`](ARCHITECTURE.md).

## Sumário

- [Pré-requisitos](#pré-requisitos)
- [Subindo tudo com Docker Compose](#subindo-tudo-com-docker-compose)
- [Rodando a aplicação fora do Docker](#rodando-a-aplicação-fora-do-docker)
- [Variáveis de ambiente](#variáveis-de-ambiente)
- [Filas SQS](#filas-sqs)
- [Migrations](#migrations)
- [Autenticação e identidades de teste](#autenticação-e-identidades-de-teste)
- [Exemplos de chamadas](#exemplos-de-chamadas)
- [Testes](#testes)
- [Observabilidade](#observabilidade)
- [Estrutura do projeto](#estrutura-do-projeto)

## Pré-requisitos

- Docker com Compose v2 (`docker compose version`)
- Go 1.27 — só para rodar os testes e as ferramentas fora dos containers

Nenhuma CLI extra é necessária: as migrations são embarcadas no binário `migrate` com
`//go:embed`, e as filas são criadas pelo próprio LocalStack no boot.

## Subindo tudo com Docker Compose

```sh
cp .env.example .env
docker compose up --build
```

Compose sobe, nesta ordem e por healthcheck: **PostgreSQL**, **Keycloak** (que importa
`keycloak/realm.json` com o realm, os clients e as identidades de teste) e **LocalStack** (que
executa `localstack/init-queues.sh`, criando as três filas FIFO com redrive e políticas de
acesso). Em seguida o serviço `migrate` aplica as migrations e sai; só então a API sobe. Nenhum
passo manual.

| Serviço | Endereço no host |
| --- | --- |
| API | `http://localhost:8080` |
| Keycloak | `http://localhost:8081` (admin `admin`/`admin`) |
| LocalStack | `http://localhost:4566` |
| PostgreSQL | `localhost:5432` (`wagering`/`wagering`) |

```sh
curl -s http://localhost:8080/health/live    # processo de pé, sem tocar dependências
curl -s http://localhost:8080/health/ready   # 200 só se Postgres e a fila de entrada respondem
curl -s http://localhost:8080/metrics        # os contadores, como JSON do expvar
```

Derrubar tudo, inclusive o volume do banco:

```sh
docker compose down -v
```

### Múltiplas instâncias

O serviço `api` publica uma **faixa** de portas em vez de uma só, então as réplicas sobem lado a
lado sem conflito:

```sh
docker compose up --build --scale api=3
curl -s http://localhost:8080/health/ready
curl -s http://localhost:8082/health/ready
```

As réplicas disputam as mesmas carteiras, a mesma fila e o mesmo outbox; nenhuma depende de
estado em memória. A porta `8081` da faixa coincide com a do Keycloak — se ele já estiver
publicado no host, mude a faixa antes de escalar, ou use a suíte de sistema descrita em
[Testes](#testes), que sobe os três processos por conta própria.

## Rodando a aplicação fora do Docker

Suba só a infraestrutura:

```sh
docker compose up postgres keycloak localstack
```

Rode a API no host, trocando os nomes de serviço do Compose pelos endereços publicados:

```sh
export $(grep -v '^#' .env.example | xargs)
export DATABASE_URL='postgres://wagering:wagering@localhost:5432/wagering?sslmode=disable'
export SQS_ENDPOINT=http://localhost:4566
export OIDC_DISCOVERY_URL=http://localhost:8081/realms/wagering
export SQS_WAGER_TRANSACTIONS_QUEUE_URL=http://localhost:4566/000000000000/wager-transactions.fifo
export SQS_WAGER_TRANSACTIONS_DLQ_URL=http://localhost:4566/000000000000/wager-transactions-dlq.fifo
export SQS_EVENTS_QUEUE_URL=http://localhost:4566/000000000000/wager-events.fifo

make migrate-up
make run
```

`OIDC_ISSUER_URL` não muda: `KC_HOSTNAME` fixa o issuer, então o token é o mesmo nos dois modos.

## Variáveis de ambiente

Toda configuração vem do ambiente. `.env.example` traz a lista completa com valores locais e
nenhum segredo real; o Compose lê o `.env` automaticamente. As obrigatórias não têm default: uma
aplicação financeira não deve assumir silenciosamente um banco, um issuer ou uma fila.

| Variável | Default | Para quê |
| --- | --- | --- |
| `APP_ENV` | `local` | Nome do ambiente, carimbado em toda linha de log |
| `HTTP_ADDR` | `:8080` | Endereço de escuta da API |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` ou `error` |
| `DATABASE_URL` | — **obrigatória** | String de conexão do PostgreSQL |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `10` / `1` | Limites do pool de conexões |
| `DB_LOCK_TIMEOUT` | `3s` | Espera máxima por uma carteira disputada antes de responder `503` |
| `SHUTDOWN_TIMEOUT` | `15s` | Orçamento do shutdown inteiro no `SIGTERM` |
| `WORKER_DRAIN_TIMEOUT` | `5s` | Cota de cada worker dentro desse orçamento, para que nenhum gaste tudo |
| `STARTUP_TIMEOUT` | `15s` | Orçamento das checagens de dependência no boot |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | Proteção contra headers lentos |
| `OIDC_ISSUER_URL` | — **obrigatória** | Issuer como aparece nos tokens |
| `OIDC_DISCOVERY_URL` | o issuer | Onde metadata e JWKS são buscados |
| `OIDC_AUDIENCE` | — **obrigatória** | Audience que os tokens precisam carregar |
| `SQS_WAGER_TRANSACTIONS_QUEUE_URL` | — **obrigatória** | Fila que o consumidor lê |
| `SQS_WAGER_TRANSACTIONS_DLQ_URL` | — **obrigatória** | DLQ das mensagens intratáveis |
| `SQS_EVENTS_QUEUE_URL` | — **obrigatória** | Fila para onde o outbox publica |
| `OUTBOX_POLL_INTERVAL` | `1s` | Espera entre ciclos de publicação |
| `OUTBOX_PUBLISH_WINDOW` | `10s` | Prazo de um ciclo, e o teto de uma linha reivindicada |
| `OUTBOX_BATCH_SIZE` | `100` | Linhas reivindicadas por ciclo |
| `REFERENCE_POLL_INTERVAL` | `1s` | Espera entre varreduras de referências pendentes |
| `REFERENCE_BATCH_SIZE` | `100` | Reversões varridas por ciclo |
| `REFERENCE_TTL` | `24h` | Quanto tempo uma reversão espera a referência antes de ser rejeitada |
| `AWS_REGION` | `us-east-1` | Região que o cliente SQS assina |
| `SQS_ENDPOINT` | vazio | Endereço do LocalStack; vazio significa SQS real |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | — | Credenciais do broker; `test`/`test` contra o LocalStack |

A validação acontece de uma vez no boot: uma configuração inválida derruba o processo antes de
montar o grafo do Fx e reporta **todos** os problemas numa mensagem só.

## Filas SQS

`localstack/init-queues.sh` roda no boot do LocalStack e provisiona três filas FIFO — nenhum
comando manual é necessário:

| Fila | Papel | `MessageGroupId` | `MessageDeduplicationId` |
| --- | --- | --- | --- |
| `wager-transactions.fifo` | entrada das operações | a carteira | do produtor, por mensagem |
| `wager-transactions-dlq.fifo` | destino da redrive policy | o grupo original | o `messageId` |
| `wager-events.fifo` | saída dos eventos de integração | a carteira | o `eventId` |

A fila de entrada tem `VisibilityTimeout` de 30s e `maxReceiveCount` 3 apontando para a DLQ.
Cada fila recebe uma política de acesso por principal com a ação mínima necessária. Conferir o
que subiu:

```sh
docker compose exec localstack awslocal sqs list-queues
```

## Migrations

Onze migrations versionadas em `migrations/`, como `NNNN_nome.up.sql` / `.down.sql`, embarcadas
no binário — ele não precisa dos arquivos em runtime.

```sh
export DATABASE_URL='postgres://wagering:wagering@localhost:5432/wagering?sslmode=disable'

go run ./cmd/migrate up        # aplica tudo que estiver pendente
go run ./cmd/migrate down      # reverte tudo
go run ./cmd/migrate steps -1  # reverte exatamente uma
go run ./cmd/migrate version   # versão atual, e se está dirty
```

`make migrate-up` e `make migrate-down` embrulham a aplicação e a reversão de uma. No Compose a
aplicação é automática (serviço `migrate`); para reverter lá dentro,
`docker compose run --rm migrate down`. Uma migration que falha deixa o schema *dirty*;
`version` reporta isso, e a situação precisa ser resolvida à mão antes de seguir.

## Autenticação e identidades de teste

Toda rota de carteira e de operação exige um access token OIDC válido; os health checks e as
métricas são públicos e não expõem dado financeiro. O realm `wagering` é importado pelo Compose
com estes clients `client_credentials` — segredos locais, todos no formato `<clientId>-secret`:

| Client | Escopo | Pode |
| --- | --- | --- |
| `internal-service` | `wallets` | abrir carteira, ler carteira e ledger, reconciliar |
| `provider-a`, `provider-b` | `wagering` + claim `provider_id` | submeter e consultar **apenas as próprias** operações |
| `provider-expiring` | `wagering` | o mesmo, com token de um segundo — existe para o teste de expiração |
| `outsider` | nenhum | nada; o token tem outra audiência, para o teste de audiência |

`wagering-api` é o resource server: nunca pede token, apenas nomeia a audiência que a API aceita.
`KC_HOSTNAME` fixa o issuer em `http://localhost:8081/realms/wagering`, de modo que um token
pedido do host é o mesmo token que o container da API valida; o container busca o JWKS pela rede
do Compose, que é para o que serve `OIDC_DISCOVERY_URL`.

```sh
token() {
  curl -s http://localhost:8081/realms/wagering/protocol/openid-connect/token \
    -d grant_type=client_credentials -d client_id="$1" -d client_secret="$1-secret" \
  | sed -E 's/.*"access_token":"([^"]+)".*/\1/'
}
INTERNAL=$(token internal-service)
PROVIDER=$(token provider-a)
```

Token ausente, malformado, expirado ou com audiência errada recebe `401` com
`WWW-Authenticate: Bearer`; um token de provedor numa rota de carteira recebe `403`, e o interno
numa rota de operação também. Todas as respostas de erro são `application/problem+json` e
nenhuma carrega dado de carteira.

## Exemplos de chamadas

### Abrir e ler uma carteira

```sh
curl -s -X POST http://localhost:8080/wallets \
  -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
  -d '{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
       "initialBalance":{"amount":"1000.00","currency":"BRL"}}'
# -> {"id":"...","playerId":"...","balance":{"amount":"1000.00","currency":"BRL"},"version":1}

curl -s http://localhost:8080/wallets/$WALLET -H "Authorization: Bearer $INTERNAL"
curl -s "http://localhost:8080/wallets/$WALLET/ledger?limit=50" -H "Authorization: Bearer $INTERNAL"
curl -s "http://localhost:8080/wallets/$WALLET/ledger?limit=50&cursor=$CURSOR" -H "Authorization: Bearer $INTERNAL"
```

A abertura com saldo positivo cria, no mesmo commit, a carteira, uma transação `OPENING`
`PROCESSED`, o lançamento de crédito e os eventos de outbox. Abrir uma segunda carteira para o
mesmo jogador e moeda responde `409`. O ledger pagina por cursor opaco com ordenação estável, e
a resposta traz o `nextCursor` da próxima página.

### Submeter uma operação

```sh
curl -s -X POST http://localhost:8080/wagering/transactions \
  -H "Authorization: Bearer $PROVIDER" -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: provider-a:transaction-123' \
  -d '{"providerId":"provider-a","externalTransactionId":"transaction-123",
       "playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","walletId":"'"$WALLET"'",
       "roundId":"round-987","gameId":"fortune-chimp","kind":"BET",
       "money":{"amount":"25.00","currency":"BRL"}}'
# -> {"transactionId":"...","status":"PROCESSED","balance":{"amount":"975.00","currency":"BRL"},"idempotentReplay":false}
```

O header `Idempotency-Key` é obrigatório e nunca é substituído por um valor calculado pelo
servidor. Reenviar a mesma chamada devolve o resultado persistido com `idempotentReplay: true` e
não debita nada; a mesma chave com corpo diferente devolve `409`. Para `REFUND` e `ROLLBACK`,
acrescente `referenceExternalTransactionId` ao corpo — se a referência ainda não chegou, a
operação é registrada como `PENDING_REFERENCE` e respondida `202`, e o worker de referências
assume a espera.

Consultas:

```sh
curl -s http://localhost:8080/wagering/transactions/$TRANSACTION -H "Authorization: Bearer $PROVIDER"
curl -s http://localhost:8080/providers/provider-a/wagering/transactions/transaction-123 \
  -H "Authorization: Bearer $PROVIDER"
```

Os códigos principais — a tabela completa, por classe de erro, está em
[`ARCHITECTURE.md`](ARCHITECTURE.md), em "Contratos HTTP", e os `failureCode` de cada
rejeição em "Máquina de estados e códigos de falha":

| Situação | Status |
| --- | --- |
| Processada, ou replay de uma processada | `200` |
| Reversão esperando a referência | `202` |
| Entrada inválida, `Idempotency-Key` ausente, `kind` inutilizável | `400` |
| Token não autoriza o `providerId` do corpo | `403` |
| Chave reusada com outro conteúdo, ou operação reenviada sob segunda chave | `409` |
| Rejeição de negócio — o `code` é o `failureCode` estável | `422` |
| Carteira disputada além do `DB_LOCK_TIMEOUT`, ou dependência indisponível | `503` + `Retry-After` |

### Reconciliação

```sh
curl -s -X POST http://localhost:8080/wallets/$WALLET/reconciliation -H "Authorization: Bearer $INTERNAL"
# -> {"walletId":"...","storedBalance":{...},"calculatedBalance":{...},
#     "difference":{"amount":"0.00","currency":"BRL"},"consistent":true,"checkedEntries":2}
```

Uma única consulta lê o saldo armazenado ao lado do saldo reconstruído do ledger, de modo que um
movimento que comita no meio da leitura não apareça como divergência. A reconciliação nunca altera
saldo; uma divergência real vai para o corpo, para o log e para a métrica
`reconciliation_divergences`.

### A mesma operação por SQS

O consumidor lê `wager-transactions.fifo` neste envelope:

```json
{
  "messageId": "msg-123",
  "type": "WagerTransactionRequested",
  "occurredAt": "2026-09-08T12:00:00.000Z",
  "data": {
    "providerId": "provider-a",
    "externalTransactionId": "transaction-123",
    "idempotencyKey": "provider-a:transaction-123",
    "playerId": "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
    "walletId": "0192f291-27dd-7d3f-8071-5f8685deef37",
    "roundId": "round-987",
    "gameId": "fortune-chimp",
    "kind": "BET",
    "money": { "amount": "25.00", "currency": "BRL" }
  }
}
```

```sh
docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WALLET" --message-deduplication-id msg-123 \
  --message-body "$(cat mensagem.json)"
```

A mensagem é decodificada nos mesmos parâmetros que o handler HTTP monta e entregue ao mesmo caso
de uso, então o resultado — inclusive a idempotência — é idêntico entre os dois transportes. O
registro de inbox é escrito no mesmo commit da mudança de domínio, e a mensagem só é removida
depois desse commit. O que acontece em reentrega, conflito de hash, rejeição de negócio e falha
transitória está em [`ARCHITECTURE.md`](ARCHITECTURE.md), em "Inbox e outbox" e "Contratos das filas".

## Testes

Só precisam de Go:

```sh
go test ./...        # 182 testes de unidade, sem containers
go test -race ./...  # os mesmos, sob o detector de race
go vet ./...
gofmt -l .           # não imprime nada quando a formatação está limpa
```

### Preparando as dependências dos testes

As suítes abaixo sobem **containers reais** de PostgreSQL, Keycloak e LocalStack por conta
própria, via testcontainers — não reaproveitam o ambiente do Compose e nunca usam mocks.
Basta um Docker rodando e as imagens disponíveis; para pré-baixá-las e não pagar isso dentro do
timeout do teste:

```sh
docker pull postgres:17-alpine
docker pull quay.io/keycloak/keycloak:26.4
docker pull localstack/localstack:4
```

### Integração (build tag `integration`)

```sh
go test -race -tags=integration -timeout 20m ./...
```

Cobre migrations `up` e `down`, constraints e imutabilidade do ledger, atomicidade financeira,
inbox e reentrega, outbox concorrente, retry, DLQ, a integração real com o IdP (token ausente,
inválido, expirado, audiência errada, isolamento entre provedores) e a composição do Fx com seu
start e stop.

### Múltiplas instâncias e simulações de falha (build tags `integration,system`)

`test/system` compila a API com o detector de race e roda **três processos independentes** contra
containers compartilhados, para que as garantias de concorrência sejam demonstradas entre processos
e não entre goroutines:

```sh
go test -race -tags 'integration,system' -timeout 40m ./test/system/...
```

O que está demonstrado ali:

| Cenário | Evidência |
| --- | --- |
| A mesma aposta 50 vezes em paralelo | um único débito no ledger |
| Duas apostas de 80.00 contra saldo de 100.00 | uma processada, uma rejeitada por saldo insuficiente, saldo final 20.00, um único débito — e reenvios não mudam isso |
| Carteiras distintas | avançam em paralelo mesmo com uma delas travada |
| Consumidor morto com `SIGKILL` | a mensagem é reentregue e tratada por outra instância |
| Reversão em `PENDING_REFERENCE` cuja instância morre | outra instância assume a espera |
| Três publishers sobre um mesmo outbox | publicação recuperada, `eventId` preservado |

Todo cenário termina reconciliando contra o ledger cada carteira que tocou.

O `Makefile` embrulha tudo: `run`, `build`, `test`, `test-race`, `test-integration`,
`test-system`, `vet`, `fmt`, `migrate-up`, `migrate-down`.

## Observabilidade

- **Logs** JSON em stdout, com `correlationId`, `messageId`, `transactionId`, `walletId` e
  `providerId` quando disponíveis — nunca credenciais nem payload financeiro completo. O
  `correlationId` nasce na requisição ou na mensagem, atravessa o contexto e chega aos envelopes
  publicados.
- **Métricas** em `GET /metrics`, como JSON do `expvar`: `wager_outcomes` por status,
  `wager_duplicates`, `wallet_concurrency_conflicts`, `reference_retries`, `sqs_dead_lettered`,
  `reconciliation_divergences`, `outbox_lag_seconds` e `wager_processing` (contagem e total, para
  a latência média).
- **Health checks** em `GET /health/live` (processo) e `GET /health/ready` (pool do Postgres e
  fila de entrada), ambos públicos.

## Estrutura do projeto

```
cmd/api            raiz de composição (o wiring do Fx vive no package main) e o binário da API
cmd/migrate        CLI de migrations: up, down, steps n, version
internal/domain    núcleo de negócio — não importa Fx, HTTP, SQS nem pgx
internal/app       casos de uso; declaram as portas que os adaptadores satisfazem
internal/adapter   httpapi, postgres, sqs — só tradução
internal/worker    workers de outbox e de referências pendentes
internal/platform  config, logging, httpserver, postgres, health, auth, correlation, metrics
keycloak           realm importado pelo Compose: clients, escopos e identidades de teste
localstack         provisionamento das filas, executado no boot do LocalStack
migrations         SQL versionado, embarcado com //go:embed
test/system        a suíte multi-instância
```

| Documento | Conteúdo |
| --- | --- |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | As decisões e o que cada uma custa: dinheiro, transações, idempotência, locks, referências pendentes, reversões, inbox/outbox, autenticação, autorização, Fx, shutdown — com as interpretações adotadas e as limitações |
| [`DESAFIO.md`](DESAFIO.md) | O enunciado original, preservado sem edição |
