# Processamento Distribuído de Apostas

Serviço em Go, composto com Uber Fx, que processa operações financeiras de provedores de jogos
(`BET`, `WIN`, `LOSS`, `REFUND`, `ROLLBACK`) sobre carteiras de jogadores — por HTTP e por SQS,
pelo mesmo caso de uso — com idempotência persistente, coordenação por carteira, outbox
transacional e recuperação de falhas demonstrada com três processos reais.

| Documento | Conteúdo |
| --- | --- |
| este `README.md` | como subir, autenticar, exercitar e testar |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | as decisões, os trade-offs, as interpretações adotadas e as limitações |
| [`api/openapi.yaml`](api/openapi.yaml) | o contrato HTTP em OpenAPI 3.1 |
| [`DESAFIO.md`](DESAFIO.md) | o enunciado original, sem edição |

## Sumário

1. [Pré-requisitos](#1-pré-requisitos)
2. [Subir o ambiente](#2-subir-o-ambiente)
3. [Autenticar](#3-autenticar)
4. [Roteiro: o fluxo completo pela linha de comando](#4-roteiro-o-fluxo-completo-pela-linha-de-comando)
5. [Swagger UI](#5-swagger-ui)
6. [Três instâncias ao mesmo tempo](#6-três-instâncias-ao-mesmo-tempo)
7. [Testes](#7-testes)
8. [Testes obrigatórios e critérios desclassificatórios](#8-testes-obrigatórios-e-critérios-desclassificatórios)
9. [Referência](#9-referência) — variáveis, filas, migrations, códigos HTTP, observabilidade, rodar fora do Docker, estrutura

## 1. Pré-requisitos

- Docker com Compose v2 (`docker compose version`)
- `curl`, `jq` e `uuidgen` — só para o roteiro da seção 4
- Go 1.27 — só para os testes e para rodar a API fora do Docker

Nenhuma outra CLI: as migrations estão embarcadas no binário e as filas são criadas pelo próprio
LocalStack no boot.

## 2. Subir o ambiente

```sh
cp .env.example .env
docker compose up --build -d
docker compose ps
```

O Compose sobe, esperando o healthcheck de cada um: **PostgreSQL**, **Keycloak** (importa
`keycloak/realm.json`: realm, clients e identidades de teste), **LocalStack** (roda
`localstack/init-queues.sh`: filas FIFO, redrive e políticas de acesso), o **migrate** (aplica as
migrations e sai) e por fim a **API**. Nenhum passo manual.

| Serviço | Endereço no host |
| --- | --- |
| API | `http://localhost:8080` |
| Keycloak | `http://localhost:8081` (console admin: `admin` / `admin`) |
| LocalStack | `http://localhost:4566` |
| PostgreSQL | `localhost:5432` (`wagering` / `wagering`) |

Pronto quando isto responde `200`:

```sh
curl -s -w '  HTTP %{http_code}\n' http://localhost:8080/health/ready
```

Logs: `docker compose logs -f api`. Derrubar tudo, inclusive o banco: `docker compose down -v`.

## 3. Autenticar

Toda rota de carteira e de operação exige um access token OIDC emitido pelo Keycloak; health
checks e `/metrics` são públicos. O realm `wagering` vem com estas identidades
`client_credentials` — segredos locais, sempre `<client_id>-secret`:

| `client_id` | `client_secret` | Escopo | Pode |
| --- | --- | --- | --- |
| `internal-service` | `internal-service-secret` | `wallets` | abrir carteira, ler carteira e ledger, reconciliar |
| `provider-a` | `provider-a-secret` | `wagering` | submeter e consultar **só** operações com `providerId: provider-a` |
| `provider-b` | `provider-b-secret` | `wagering` | o mesmo, para `provider-b` |
| `provider-expiring` | `provider-expiring-secret` | `wagering` | token de 1 segundo — existe para o teste de expiração |
| `outsider` | `outsider-secret` | nenhum | nada; token de outra audiência — existe para o teste de audiência |

O `providerId` autorizado vem da claim `provider_id` do token, nunca do corpo. Um token de
provedor numa rota de carteira recebe `403`, e o interno numa rota de operação também. Token
ausente, malformado, expirado ou de outra audiência recebe `401`.

Pedir os tokens no shell — **eles vencem em 5 minutos**; se uma chamada começar a responder
`401`, rode `login` de novo:

```sh
token() {
  curl -s http://localhost:8081/realms/wagering/protocol/openid-connect/token \
    -d grant_type=client_credentials -d client_id="$1" -d client_secret="$1-secret" \
  | jq -r .access_token
}
login() {
  INTERNAL=$(token internal-service)
  PROVIDER=$(token provider-a)
  PROVIDER_B=$(token provider-b)
}
login
```

## 4. Roteiro: o fluxo completo pela linha de comando

Cole os blocos em ordem, no mesmo terminal onde rodou a seção 3 — cada um usa as variáveis e
funções do anterior. Os resultados esperados estão no texto, fora dos blocos, para que tudo o que
está dentro de um bloco possa ser colado inteiro.

### 4.1. Funções auxiliares

`open_wallet` abre uma carteira para um jogador novo e guarda `PLAYER` e `WALLET`. `submit` envia
uma operação de `provider-a` para essa carteira e imprime o corpo e o status HTTP. `RUN` prefixa os
ids externos, porque `(providerId, externalTransactionId)` é único para sempre — sem ele, repetir o
roteiro seria um replay do anterior.

```sh
API=http://localhost:8080
RUN=$(date +%s)

open_wallet() {
  PLAYER=$(uuidgen | tr '[:upper:]' '[:lower:]')
  WALLET=$(curl -s -X POST $API/wallets \
    -H "Authorization: Bearer $INTERNAL" -H 'Content-Type: application/json' \
    -d "$(jq -n --arg player "$PLAYER" --arg amount "$1" \
          '{playerId: $player, initialBalance: {amount: $amount, currency: "BRL"}}')" \
    | tee /dev/stderr | jq -r .id)
  echo "WALLET=$WALLET"
}

submit() {
  curl -s -w '  HTTP %{http_code}\n' -X POST ${API}/wagering/transactions \
    -H "Authorization: Bearer $PROVIDER" -H 'Content-Type: application/json' \
    -H "Idempotency-Key: provider-a:$RUN-$2" \
    -d "$(jq -n --arg kind "$1" --arg id "$RUN-$2" --arg amount "$3" --arg ref "${4:+$RUN-$4}" \
          --arg player "$PLAYER" --arg wallet "$WALLET" '{
      providerId: "provider-a", externalTransactionId: $id,
      playerId: $player, walletId: $wallet, roundId: "round-987", gameId: "fortune-chimp",
      kind: $kind, money: {amount: $amount, currency: "BRL"}
    } + (if $ref == "" then {} else {referenceExternalTransactionId: $ref} end)')"
}
```

Uso: `submit KIND ID_EXTERNO VALOR [ID_EXTERNO_DA_REFERÊNCIA]`.

### 4.2. Abrir e ler uma carteira

```sh
open_wallet 1000.00
curl -s $API/wallets/$WALLET -H "Authorization: Bearer $INTERNAL" | jq
```

A abertura responde `{"id":"…","playerId":"…","balance":{"amount":"1000.00","currency":"BRL"},"version":1}`.
No mesmo commit ficam a carteira, uma transação `OPENING` `PROCESSED`, o lançamento de crédito e
os eventos de outbox. Abrir uma segunda carteira para o mesmo jogador e moeda responde `409`.

### 4.3. Apostas, idempotência e rejeição

```sh
submit BET bet-1 25.00
submit BET bet-1 25.00
submit BET bet-1 30.00
submit BET bet-2 5000.00
```

1. `200`, `PROCESSED`, saldo `975.00`, `idempotentReplay: false`.
2. `200`, o **mesmo** resultado com `idempotentReplay: true` — nada debitado de novo.
3. `409`: a mesma `Idempotency-Key` com outro conteúdo.
4. `422` com `code: INSUFFICIENT_FUNDS` — rejeição de negócio, registrada e auditável.

### 4.4. `WIN` e `LOSS`

```sh
submit WIN win-1 50.00 bet-1
submit LOSS loss-1 0.00
```

`WIN` credita (saldo `1025.00`); a referência é opcional e só registrada. `LOSS` exige valor zero,
fecha como `PROCESSED` e não move dinheiro: nenhum lançamento, nenhuma mudança de versão.

### 4.5. Uma reversão que chega antes da referência

```sh
submit REFUND refund-3 10.00 bet-3
submit BET bet-3 10.00
```

O `REFUND` responde `202` com `PENDING_REFERENCE`: a aposta `bet-3` ainda não existe. A aposta
então chega e debita (`1015.00`). O worker de referências varre a espera em alguns segundos e
aplica o reembolso:

```sh
curl -s $API/providers/provider-a/wagering/transactions/$RUN-refund-3 \
  -H "Authorization: Bearer $PROVIDER" | jq '{status, failureCode, balance}'
```

Repita até ver `"status": "PROCESSED"` com saldo `1025.00`. Uma referência que nunca chega vira
`REJECTED` com `REFERENCE_NOT_FOUND` quando vence `REFERENCE_TTL`.

### 4.6. A mesma operação por SQS

O consumidor lê `wager-transactions.fifo` e entrega a mensagem ao mesmo caso de uso do HTTP:

```sh
MSG=$(jq -n --arg run "$RUN" --arg player "$PLAYER" --arg wallet "$WALLET" '{
  messageId: "\($run)-msg-1", type: "WagerTransactionRequested",
  occurredAt: "2026-09-08T12:00:00.000Z",
  data: {
    providerId: "provider-a", externalTransactionId: "\($run)-sqs-1",
    idempotencyKey: "provider-a:\($run)-sqs-1",
    playerId: $player, walletId: $wallet, roundId: "round-987", gameId: "fortune-chimp",
    kind: "BET", money: {amount: "25.00", currency: "BRL"}
  }}')
echo "$MSG" | jq

docker compose exec localstack awslocal sqs send-message \
  --queue-url http://localhost:4566/000000000000/wager-transactions.fifo \
  --message-group-id "$WALLET" --message-deduplication-id "$RUN-msg-1" \
  --message-body "$MSG"
```

Um ou dois segundos depois a operação está processada (saldo `1000.00`):

```sh
curl -s $API/providers/provider-a/wagering/transactions/$RUN-sqs-1 \
  -H "Authorization: Bearer $PROVIDER" | jq '{status, balance}'
```

O registro de inbox é gravado no mesmo commit da mudança de domínio, e a mensagem só sai da fila
depois desse commit. Mandar pela fila uma operação que já chegou por HTTP é um replay, não um
segundo débito — `TestTheSameOperationOverHTTPAndSQSAppliesOnce` prova isso.

### 4.7. Consultas, ledger paginado e isolamento entre provedores

```sh
TX=$(curl -s $API/providers/provider-a/wagering/transactions/$RUN-bet-1 \
  -H "Authorization: Bearer $PROVIDER" | jq -r .transactionId)
curl -s $API/wagering/transactions/$TX -H "Authorization: Bearer $PROVIDER" | jq

curl -s -w '  HTTP %{http_code}\n' $API/wagering/transactions/$TX -H "Authorization: Bearer $PROVIDER_B"
curl -s -w '  HTTP %{http_code}\n' $API/wallets/$WALLET -H "Authorization: Bearer $PROVIDER"
```

`provider-b` não enxerga a transação de `provider-a`, e um provedor não lê carteiras — nenhum
corpo de erro carrega dado financeiro.

```sh
PAGE=$(curl -s "$API/wallets/$WALLET/ledger?limit=4" -H "Authorization: Bearer $INTERNAL")
echo "$PAGE" | jq '.entries[] | {direction, money, balanceAfter}'
CURSOR=$(echo "$PAGE" | jq -r .nextCursor)
curl -s "$API/wallets/$WALLET/ledger?limit=4&cursor=$CURSOR" -H "Authorization: Bearer $INTERNAL" | jq
```

O ledger pagina por cursor opaco em ordem estável: 4 lançamentos na primeira página, 2 na segunda, que já não traz `nextCursor`. Uma página cheia sempre traz o cursor, mesmo que a seguinte venha vazia.

### 4.8. Reconciliação

```sh
curl -s -X POST $API/wallets/$WALLET/reconciliation -H "Authorization: Bearer $INTERNAL" | jq
```

Depois do roteiro: `storedBalance` e `calculatedBalance` iguais a `1000.00`, `difference`
`0.00`, `consistent: true` e `checkedEntries: 6` (abertura, `bet-1`, `win-1`, `bet-3`,
`refund-3`, `sqs-1`). Uma única consulta lê o saldo armazenado ao lado do reconstruído do ledger;
nada é alterado, e uma divergência real vai para o corpo, para o log e para a métrica
`reconciliation_divergences`.

## 5. Swagger UI

```sh
docker run --rm --name api-docs -p 8088:8080 \
  -e SWAGGER_JSON=/spec/openapi.yaml -v "$PWD/api:/spec" \
  swaggerapi/swagger-ui
```

Abra `http://localhost:8088` com o ambiente da seção 2 de pé. Para autenticar:

1. Clique em **Authorize**.
2. Em `client_id` e `client_secret`, use uma identidade da [seção 3](#3-autenticar) — por exemplo
   `internal-service` / `internal-service-secret`.
3. Marque **só** o escopo dessa identidade (`wallets` para o interno, `wagering` para um
   provedor) e clique em **Authorize**. O Swagger pede o token ao Keycloak e o envia em toda
   chamada de **Try it out**.
4. Para trocar de identidade — abrir a carteira como interno e apostar como `provider-a`, por
   exemplo — clique em **Logout** e repita.

A porta precisa ser `8088`: é a única origem que o Keycloak aceita para o Swagger pedir token do
navegador. `npx @redocly/cli lint api/openapi.yaml` valida a especificação. Ela é escrita à mão,
não gerada dos handlers, então uma rota nova precisa de edição lá também.

## 6. Três instâncias ao mesmo tempo

```sh
API_PORTS=8082-8084 docker compose up --build -d --scale api=3
docker compose ps api
```

Sozinha, a API fica na `8080`. Escalada, cada réplica pega uma porta da faixa `8082-8084` (a
`8081` é do Keycloak) — as três estão em uso, em qualquer ordem. Todas disputam as mesmas
carteiras, a mesma fila e o mesmo outbox, sem estado em memória.

O teste obrigatório das duas apostas de 80.00 contra 100.00, à mão, em duas instâncias diferentes
ao mesmo tempo (com as funções da seção 4.1 carregadas):

```sh
login
API=http://localhost:8082
open_wallet 100.00
API=http://localhost:8083 submit BET race-a 80.00 &
API=http://localhost:8084 submit BET race-b 80.00 &
wait
curl -s -X POST $API/wallets/$WALLET/reconciliation -H "Authorization: Bearer $INTERNAL" | jq
```

Uma aposta `200`, a outra `422 INSUFFICIENT_FUNDS`; na reconciliação, saldo `20.00`,
`consistent: true` e `checkedEntries: 2` (a abertura e um único débito).

Voltar a uma instância: `docker compose up -d --scale api=1` e `API=http://localhost:8080`. A
versão automatizada e mais rigorosa disto — com `SIGKILL`, tomada de trabalho e três
publishers — é a suíte de sistema da [seção 7](#73-múltiplas-instâncias-e-simulações-de-falha).

## 7. Testes

### 7.1. Unidade — só Go

```sh
go test ./...
go test -race ./...
go vet ./...
gofmt -l .
```

`gofmt -l .` não imprime nada quando a formatação está limpa.

### 7.2. Integração — build tag `integration`

Os testes sobem **containers reais** de PostgreSQL, Keycloak e LocalStack por conta própria, via
testcontainers. Não usam o ambiente do Compose e nunca usam mocks; basta o Docker rodando. Para
não gastar o timeout do teste baixando imagens:

```sh
docker pull postgres:17-alpine
docker pull quay.io/keycloak/keycloak:26.4
docker pull localstack/localstack:4
```

```sh
go test -race -tags=integration -timeout 20m ./...
```

Cobrem migrations `up` e `down`, constraints e imutabilidade do ledger, atomicidade financeira,
inbox e reentrega, outbox concorrente, retry, DLQ, a integração real com o IdP e a composição do
Fx com start e stop.

### 7.3. Múltiplas instâncias e simulações de falha — build tags `integration,system`

`test/system` compila a API com o detector de race e roda **três processos independentes**,
cada um com suas conexões e sua memória, contra containers compartilhados:

```sh
go test -race -tags 'integration,system' -timeout 40m ./test/system/...
```

Todo cenário termina reconciliando contra o ledger cada carteira que tocou.

### 7.4. Um teste só

```sh
go test -race -tags=integration -run '^TestTwoBetsRaceForOneBalance$' -v ./cmd/api
```

O `Makefile` embrulha os comandos: `test`, `test-race`, `test-integration`, `test-system`, `vet`,
`fmt`, além de `run`, `build`, `migrate-up` e `migrate-down`.

## 8. Testes obrigatórios e critérios desclassificatórios

Onde está a prova de cada exigência de teste do enunciado (§13) e de cada critério
desclassificatório (§14). Coluna **Tags**: `—` roda em `go test ./...`; `integration` e `system`
são as build tags das seções 7.2 e 7.3. Os testes sem pacote indicado estão em `./cmd/api`.

### 8.1. Concorrência e recuperação (§13, itens 1 a 8)

| # | Exigência | Teste | Tags |
| --- | --- | --- | --- |
| 1 | A mesma aposta 50 vezes em paralelo, um único débito | `TestSameBetInParallelDebitsOnce` | integration |
|   | … entre três processos | `TestTheSameBetFiftyTimesAcrossInstancesDebitsOnce` | system |
| 2 | Duas apostas de 80.00 contra 100.00: uma processada, uma `INSUFFICIENT_FUNDS`, saldo 20.00, um débito, reenvios não mudam nada | `TestTwoBetsRaceForOneBalance` | integration |
|   | … entre três processos | `TestTwoBetsOfEightyRaceForOneHundredAcrossInstances` | system |
| 3 | Carteiras distintas em paralelo | `TestDistinctWalletsProceedInParallel` | integration |
|   | … uma carteira travada não bloqueia outra, entre processos | `TestAHeldWalletDoesNotBlockAnother` | system |
| 4 | Os cenários relevantes com três instâncias independentes | toda a suíte `./test/system` | system |
| 5 | Consumidor interrompido depois do commit e antes de remover a mensagem; reentrega validada | `TestAMessageRedeliveredAfterACommitIsANoOp` | integration |
|   | … processo morto com `SIGKILL` | `TestAKilledConsumerLosesNoMessageAndDuplicatesNoDebit` | system |
| 6 | Dois publishers disputando o outbox; recuperação da publicação | `TestTwoPublishersNeverClaimTheSameRow`, `TestARowLostBetweenSendAndConfirmationIsRepublishedUnchanged` (`./internal/adapter/postgres`) | integration |
|   | … três publishers, `eventId` preservado | `TestThreePublishersPublishEveryEventOnce` | system |
| 7 | `REFUND`/`ROLLBACK` antes da referência: resolução posterior, ou rejeição por expiração | `TestAReversalDeliveredBeforeItsReferenceResolvesLater`, `TestAWaitExpiresIntoAReferenceNotFoundRejection` | integration |
| 8 | Reinício preserva idempotência, trabalho pendente e consistência; outra instância assume | `TestAWaitSurvivesARestartAndIsResumedByAnotherInstance` | integration |
|   | … a instância morre, outra assume, e a aposta reenviada é replay com o saldo original | `TestAPendingReferenceIsTakenOverByAnotherInstance` | system |
| — | A mesma operação por HTTP e por SQS, um único débito | `TestTheSameOperationOverHTTPAndSQSAppliesOnce`, `TestHTTPAndSQSRacingForOneOperationApplyItOnce` (as duas entradas em paralelo) | integration |

### 8.2. Autenticação e autorização (§13)

| Exigência | Teste | Tags |
| --- | --- | --- |
| Integração real com o IdP; recusa de token ausente, inválido, expirado, de outra audiência | `TestVerifyRefusesTokensThisAPIMustNotAccept` (`./internal/platform/auth`), `TestGuardRefusesUnusableCredentials` (`./internal/adapter/httpapi`) | integration / — |
| Isolamento entre provedores, inclusive em consultas e replays | `TestProvidersAreIsolated` | integration |
| Operações internas restritas ao serviço interno | `TestWalletRoutesAcceptOnlyTheInternalService` | integration |
| Nenhum efeito financeiro nem exposição de dados sem autorização | `TestUnauthorizedReadExposesNoWalletData` | integration |

### 8.3. Critérios desclassificatórios (§14)

| Desclassifica se… | O que prova que não acontece | Tags |
| --- | --- | --- |
| endpoints de negócio sem autenticação efetiva | `TestWalletRoutesAcceptOnlyTheInternalService`, `TestVerifyRefusesTokensThisAPIMustNotAccept` | integration |
| acesso não autorizado a operações ou transações | `TestProvidersAreIsolated`, `TestUnauthorizedReadExposesNoWalletData` | integration |
| cálculo monetário em ponto flutuante | `TestInternalContainsNoFloat` (`./internal/domain`) — varre todo `internal/` com `go/ast` | — |
| saldo negativo por concorrência | `TestTwoBetsRaceForOneBalance`, `TestTwoBetsOfEightyRaceForOneHundredAcrossInstances`; o `CHECK` no banco por `TestSchemaEnforcesTheFinancialInvariants` (`./internal/adapter/postgres`) | integration, system |
| movimentação de saldo duplicada | `TestSameBetInParallelDebitsOnce`, `TestARedeliveredMessageDebitsOnce`, `TestTheSameOperationOverHTTPAndSQSAppliesOnce`, `TestHTTPAndSQSRacingForOneOperationApplyItOnce`, `TestTwoReversalsRacingForOneBetReturnItOnce`, `TestTheSchemaRefusesASecondSuccessfulReversal` | integration |
| idempotência só em memória | `TestAWaitSurvivesARestartAndIsResumedByAnotherInstance`, `TestAPendingReferenceIsTakenOverByAnotherInstance` | integration, system |
| depender de uma única instância | toda a suíte `./test/system` | system |
| publicar antes do commit | `TestOutcomesWriteTheirEventsToTheOutbox`, `TestARowLostBetweenSendAndConfirmationIsRepublishedUnchanged` | integration |
| ledger não auditável | `TestSchemaEnforcesTheFinancialInvariants` (`UPDATE`/`DELETE` recusados pelo banco), `TestLedgerEntryHasNoMutatingMethods` (`./internal/domain/wallet`), `TestReconciliationAgreesAfterMixedOperationsAndChangesNothing` | integration / — |
| PostgreSQL, SQS e IdP trocados por mocks nos testes | as suítes `integration` e `system` sobem os três em containers reais | integration, system |

Rodar só os desclassificatórios:

```sh
go test -race ./internal/domain/... ./internal/adapter/httpapi/...
go test -race -tags=integration -timeout 20m -v ./cmd/api ./internal/adapter/postgres ./internal/platform/auth \
  -run 'TestWalletRoutesAcceptOnlyTheInternalService|TestVerifyRefusesTokensThisAPIMustNotAccept|TestProvidersAreIsolated|TestUnauthorizedReadExposesNoWalletData|TestTwoBetsRaceForOneBalance|TestSchemaEnforcesTheFinancialInvariants|TestSameBetInParallelDebitsOnce|TestARedeliveredMessageDebitsOnce|TestTheSameOperationOverHTTPAndSQSAppliesOnce|TestHTTPAndSQSRacingForOneOperationApplyItOnce|TestTwoReversalsRacingForOneBetReturnItOnce|TestTheSchemaRefusesASecondSuccessfulReversal|TestAWaitSurvivesARestartAndIsResumedByAnotherInstance|TestOutcomesWriteTheirEventsToTheOutbox|TestARowLostBetweenSendAndConfirmationIsRepublishedUnchanged|TestReconciliationAgreesAfterMixedOperationsAndChangesNothing'
go test -race -tags 'integration,system' -timeout 40m ./test/system/...
```

## 9. Referência

### 9.1. Códigos HTTP

A tabela completa, por classe de erro, está em [`ARCHITECTURE.md`](ARCHITECTURE.md), em
"Contratos HTTP", e os `failureCode` em "Máquina de estados e códigos de falha". Toda resposta de
erro é `application/problem+json` (RFC 9457).

| Situação | Status |
| --- | --- |
| Processada, ou replay de uma processada | `200` |
| Reversão esperando a referência | `202` |
| Entrada inválida, `Idempotency-Key` ausente, `kind` inutilizável | `400` |
| Token ausente, inválido, expirado ou de outra audiência | `401` |
| Token não autoriza a rota ou o `providerId` do corpo | `403` |
| Chave reusada com outro conteúdo, operação reenviada sob outra chave, carteira duplicada | `409` |
| Rejeição de negócio — `code` é o `failureCode` estável | `422` |
| Carteira disputada além do `DB_LOCK_TIMEOUT`, ou dependência indisponível | `503` + `Retry-After` |

### 9.2. Variáveis de ambiente

Toda configuração vem do ambiente. `.env.example` traz a lista completa com valores locais e
nenhum segredo real; o Compose lê o `.env` sozinho. As obrigatórias não têm default: uma aplicação
financeira não deve assumir em silêncio um banco, um issuer ou uma fila. Uma configuração inválida
derruba o processo no boot, antes de montar o grafo do Fx, reportando **todos** os problemas numa
mensagem só.

| Variável | Default | Para quê |
| --- | --- | --- |
| `APP_ENV` | `local` | Nome do ambiente, carimbado em toda linha de log |
| `HTTP_ADDR` | `:8080` | Endereço de escuta da API |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn` ou `error` |
| `DATABASE_URL` | — **obrigatória** | String de conexão do PostgreSQL |
| `DB_MAX_CONNS` / `DB_MIN_CONNS` | `10` / `1` | Limites do pool de conexões |
| `DB_LOCK_TIMEOUT` | `3s` | Espera máxima por uma carteira disputada antes de responder `503` |
| `SHUTDOWN_TIMEOUT` | `15s` | Orçamento do shutdown inteiro no `SIGTERM` |
| `WORKER_DRAIN_TIMEOUT` | `5s` | Cota de cada worker dentro desse orçamento |
| `STARTUP_TIMEOUT` | `15s` | Orçamento das checagens de dependência no boot |
| `HTTP_READ_HEADER_TIMEOUT` | `5s` | Proteção contra headers lentos |
| `OIDC_ISSUER_URL` | — **obrigatória** | Issuer como aparece nos tokens |
| `OIDC_DISCOVERY_URL` | o issuer | Onde metadata e JWKS são buscados |
| `OIDC_AUDIENCE` | — **obrigatória** | Audiência que os tokens precisam carregar |
| `SQS_WAGER_TRANSACTIONS_QUEUE_URL` | — **obrigatória** | Fila que o consumidor lê |
| `SQS_WAGER_TRANSACTIONS_DLQ_URL` | — **obrigatória** | DLQ das mensagens intratáveis |
| `SQS_EVENTS_QUEUE_URL` | — **obrigatória** | Fila para onde o outbox publica |
| `OUTBOX_POLL_INTERVAL` | `1s` | Espera entre ciclos de publicação |
| `OUTBOX_PUBLISH_WINDOW` | `10s` | Prazo de um ciclo, e o teto de uma linha reivindicada |
| `OUTBOX_BATCH_SIZE` | `100` | Linhas reivindicadas por ciclo |
| `REFERENCE_POLL_INTERVAL` | `1s` | Espera entre varreduras de referências pendentes |
| `REFERENCE_BATCH_SIZE` | `100` | Reversões varridas por ciclo |
| `REFERENCE_TTL` | `24h` | Quanto uma reversão espera a referência antes de ser rejeitada |
| `AWS_REGION` | `us-east-1` | Região que o cliente SQS assina |
| `SQS_ENDPOINT` | vazio | Endereço do LocalStack; vazio significa SQS real |
| `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` | — | Credenciais do broker; `test`/`test` contra o LocalStack |

### 9.3. Filas SQS

`localstack/init-queues.sh` roda no boot do LocalStack e provisiona três filas FIFO:

| Fila | Papel | `MessageGroupId` | `MessageDeduplicationId` |
| --- | --- | --- | --- |
| `wager-transactions.fifo` | entrada das operações | a carteira | do produtor, por mensagem |
| `wager-transactions-dlq.fifo` | destino da redrive policy | o grupo original | o `messageId` |
| `wager-events.fifo` | saída dos eventos de integração | a carteira | o `eventId` |

A fila de entrada tem `VisibilityTimeout` de 30s e `maxReceiveCount` 3 apontando para a DLQ; cada
fila tem uma política de acesso por principal com a ação mínima necessária. Reentrega, conflito
de hash, rejeição de negócio e falha transitória estão em [`ARCHITECTURE.md`](ARCHITECTURE.md),
em "Inbox e outbox" e "Contratos das filas".

```sh
docker compose exec localstack awslocal sqs list-queues
docker compose exec localstack awslocal sqs receive-message --max-number-of-messages 10 \
  --queue-url http://localhost:4566/000000000000/wager-events.fifo
```

O segundo comando mostra os eventos que o outbox publicou durante o roteiro.

### 9.4. Migrations

Doze migrations em `migrations/`, como `NNNN_nome.up.sql` / `.down.sql`, embarcadas no binário.
No Compose o serviço `migrate` as aplica sozinho. Para operar à mão:

| Onde | Aplicar | Reverter uma | Reverter tudo | Versão |
| --- | --- | --- | --- | --- |
| Compose | `docker compose run --rm migrate up` | `docker compose run --rm migrate steps -1` | `docker compose run --rm migrate down` | `docker compose run --rm migrate version` |
| Host | `make migrate-up` | `make migrate-down` | `go run ./cmd/migrate down` | `go run ./cmd/migrate version` |

No host, `DATABASE_URL` aponta para `localhost:5432` por default no `Makefile`. Uma migration que
falha deixa o schema *dirty*; `version` reporta isso, e a situação precisa ser resolvida à mão.

### 9.5. Observabilidade

- **Logs** JSON em stdout, com `correlationId`, `messageId`, `transactionId`, `walletId` e
  `providerId` quando disponíveis — nunca credenciais nem payload financeiro completo. Mande
  `X-Correlation-Id` numa requisição para segui-la nos logs e nos eventos que ela causou.
- **Métricas** em `GET /metrics`, como JSON do `expvar`: `wager_outcomes` por status,
  `wager_duplicates`, `wallet_concurrency_conflicts`, `reference_retries`, `sqs_dead_lettered`,
  `reconciliation_divergences`, `outbox_lag_seconds` e `wager_processing` (contagem e total).
- **Health checks** públicos: `GET /health/live` (o processo) e `GET /health/ready` (pool do
  Postgres e fila de entrada).

```sh
curl -s http://localhost:8080/metrics | jq '{wager_outcomes, wager_duplicates, outbox_lag_seconds}'
```

### 9.6. Rodar a API fora do Docker

Para depurar a API no host. Suba só a infraestrutura — sem o serviço `api`, que ocuparia a
porta 8080:

```sh
docker compose up -d postgres keycloak localstack
```

Em outro terminal, carregue o `.env.example` e troque os nomes de serviço do Compose pelos
endereços publicados no host:

```sh
set -a; . ./.env.example; set +a
export DATABASE_URL='postgres://wagering:wagering@localhost:5432/wagering?sslmode=disable'
export OIDC_DISCOVERY_URL=http://localhost:8081/realms/wagering
export SQS_ENDPOINT=http://localhost:4566
export SQS_WAGER_TRANSACTIONS_QUEUE_URL=http://localhost:4566/000000000000/wager-transactions.fifo
export SQS_WAGER_TRANSACTIONS_DLQ_URL=http://localhost:4566/000000000000/wager-transactions-dlq.fifo
export SQS_EVENTS_QUEUE_URL=http://localhost:4566/000000000000/wager-events.fifo
make migrate-up
make run
```

`OIDC_ISSUER_URL` não muda: `KC_HOSTNAME` fixa o issuer em `http://localhost:8081/realms/wagering`,
então o token é o mesmo nos dois modos. As seções 3 a 5 funcionam igual.

### 9.7. Estrutura do projeto

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
api                a especificação OpenAPI da superfície HTTP
test/system        a suíte multi-instância
```
