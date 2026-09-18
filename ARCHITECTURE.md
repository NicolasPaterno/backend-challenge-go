# ARCHITECTURE.md

Decisões de arquitetura do serviço, e o que cada uma custa. Escrito contra
[`DESAFIO.md`](DESAFIO.md), o enunciado original. `A.n` é uma interpretação adotada onde o
enunciado deixa a escolha em aberto; todas estão indexadas na seção 14.

Como reproduzir o ambiente, obter um token e rodar as suítes está no [`README.md`](README.md).

## Stack e organização

| Responsabilidade | Escolha | Motivo |
| --- | --- | --- |
| HTTP | `net/http` puro (`ServeMux` com padrões por método) | Go 1.22+ roteia `POST /wallets/{id}` nativamente; um router externo não acrescentaria nada aqui. |
| Banco | pgx v5 com SQL escrito à mão | o enunciado exige transações, locks e constraints verificáveis. `sqlc` esconderia `FOR NO KEY UPDATE` e `SKIP LOCKED` atrás de geração; GORM esconderia a fronteira transacional, que é justamente o que o enunciado manda documentar. |
| Dinheiro | `int64` em unidades mínimas | o enunciado proíbe float em qualquer etapa; overflow é checado explicitamente. |
| Migrations | golang-migrate, embutidas com `//go:embed` | Versionadas, com `up` e `down`, aplicáveis pelo binário `cmd/migrate` ou pelo Compose. |
| Mensageria | SQS FIFO via LocalStack (A.2) | o enunciado oferece LocalStack ou MiniStack; FIFO, DLQ e redrive são exercidos em container real. |
| IdP | Keycloak, `client_credentials`, realm importado | o enunciado recomenda; o realm sobe junto do Compose, sem passo manual. |
| Token | `go-oidc` | Discovery, cache de JWKS e validação de claims por biblioteca madura. |
| Logs | `log/slog` em JSON | o enunciado pede JSON estruturado; está na biblioteca padrão. |
| Métricas | `expvar` em `GET /metrics` | o enunciado nomeia oito coisas a medir e nenhum formato. Custo: sem histograma, logo sem percentis. |
| Testes | `testing` + testcontainers-go | o enunciado proíbe substituir a infraestrutura por mocks. |

`internal/domain` é o núcleo e não importa Fx, HTTP, SQS nem pgx; `internal/app` declara as
portas; `internal/adapter` as satisfaz; `internal/platform` cuida de config, log, servidor, pool,
auth e métricas; `internal/worker` roda outbox e referências pendentes; `cmd/api` é o único lugar
que conhece Fx.

## Invariantes garantidas pelo banco

Nenhuma invariante financeira depende só do código Go.

| Constraint ou índice | Regra |
| --- | --- |
| `wallets.balance_minor CHECK (>= 0)` | saldo negativo não sobrevive a um commit |
| `wallets_player_currency_unique` | uma carteira por `(playerId, currency)` |
| `wallet_ledger_entries_wallet_transaction_unique` | um lançamento por `(wallet_id, transaction_id)` |
| `wallet_ledger_entries_equation` | `balanceAfter = balanceBefore ± amount`, conforme a direção |
| `wallet_ledger_entries_append_only` (trigger) | `UPDATE`/`DELETE` levantam `restrict_violation`, inclusive para sessões administrativas |
| `wager_transactions_opening_unique` (parcial) | uma `OPENING` por carteira, qualquer que seja o UUID |
| `wager_transactions_origin_fields` | `INTERNAL ⇔ OPENING` com as colunas externas `NULL`; `EXTERNAL ⇒` não-`OPENING` com elas preenchidas |
| `wager_transactions_provider_key_unique` | um registro por `(provider_id, idempotency_key)` |
| `wager_transactions_provider_external_unique` | uma operação por `(provider_id, external_transaction_id)`, sob qualquer chave |
| `wager_transactions_reference_reversal_unique` (parcial) | uma reversão bem-sucedida consome a referência, seja `REFUND` ou `ROLLBACK` |
| `inbox_messages` PK `(consumer_name, message_id)` | uma mensagem tratada uma vez |

Uma ausência deliberada: `wager_transactions.wallet_id` não tem chave estrangeira (migration
`0007`). Sem isso, uma `BET` apontando para carteira inexistente não seria gravável, e é devido um
evento de rejeição a toda recusa definitiva — que precisa de um agregado para existir (A.7). O
ledger continua referenciando `wallets`, então dinheiro segue sem poder se mover contra carteira
que não existe.

## 1. Money

`money.Money` é imutável: `int64` de unidades mínimas mais `Currency`. O enunciado permite unidades
mínimas ou decimal exato; o fluxo é soma, subtração e comparação numa escala só, onde uma
biblioteca decimal não compraria nada e acrescentaria dependência ao domínio.

**Formas aceitas e a normalização que o hash enxerga.** O padrão é
`^-?[0-9]+(\.[0-9]{1,2})?$`; depois: a fração é completada com zeros até duas casas, o ponto cai, e
o resultado é lido como `int64`. `"25"`, `"25.0"`, `"025"` e `"025.00"` são todos `2500`. Nada é
arredondado — escala excedente é recusada, nunca truncada. A saída é sempre `"25.00"`.

Recusados: `""`, `NaN`, `Infinity`, notação científica, escala excedente (`25.000`), `"25."`,
`".00"`, `"+25.00"`, espaços, separador de milhar, negativo, e zero negativo em qualquer grafia
(A.3.1).

Por isso o hash de idempotência usa `amountMinor` e `currency`, **nunca o texto recebido**: hashear
texto faria a mesma operação reenviada como `"25"` e depois `"25.00"` sob uma só `Idempotency-Key`
parecer conflito de payload e responder `409` em vez do replay idempotente. Pela mesma razão, a regra de
`LOSS` é `Money.IsZero()` e a igualdade de valor na reversão é `Money.Cmp`, não comparação de
string.

**Sinal.** `Parse` e `UnmarshalJSON` recusam negativo (entrada financeira externa); `Sub` e
`Neg` o produzem (diferenças internas); `FromMinor` o aceita (reidratação); `MarshalJSON` e
`String` o renderizam — porque a `difference` da reconciliação é saldo armazenado menos
reconstruído e pode ser negativa. Saldo de carteira negativo continua impossível: isso é
regra do agregado e `CHECK` do schema.

**Persistência.** Toda coluna monetária é `BIGINT` de unidades mínimas ao lado de uma coluna de
moeda `TEXT` sob `CHECK (currency ~ '^[A-Z]{3}$')` — o mesmo `int64` do domínio, sem conversão
decimal no driver para dar errado. O `CHECK` é só de formato; a pertinência ao registro de moedas é
reaplicada na leitura por `money.ParseCurrency`.

**Limitações.** A escala é dois para toda moeda: JPY (nenhuma casa) e KWD (três) seriam gravadas
errado. Só BRL, EUR e USD liquidam (A.3.2) — EUR e USD existem para que os testes de moeda
incompatível usem códigos reais. `"brl"` minúsculo é recusado. O caminho de upgrade está marcado
com `ponytail:` em `currency.go`: `supported` vira `map[Currency]int` de expoentes.

## 2. Carteira e controle de concorrência

**A escolha (A.6):** toda operação que pode mover saldo abre uma transação SQL, toma
`SELECT … FROM wallets WHERE id = $1 FOR NO KEY UPDATE`, deixa o agregado decidir sobre esse
snapshot travado, e commita a transação, o lançamento e o saldo juntos. O `UPDATE` do saldo ainda
carrega `AND version = <a versão lida sob o lock>`, e zero linhas afetadas é erro.

Os dois existem porque as duas exigências querem coisas diferentes: o lock evita a atualização perdida; o
predicado de versão e o `CHECK (balance_minor >= 0)` fazem com que nenhuma das duas garantias
dependa de um chamador futuro lembrar de travar. O enunciado pede que a mudança de saldo fique sob
controle do agregado **e** da transação SQL — os dois, não um ou outro.

| Alternativa | Por que não |
| --- | --- |
| `UPDATE` atômico condicional (`SET balance = balance - $2 WHERE balance >= $2`) | Satisfaz a metade SQL da exigência e abandona a metade do agregado: `wallet.Debit` vira decorativo e a não-negatividade migra para um `WHERE`. Ainda obrigaria a reconstruir o lançamento a partir de `RETURNING`. É a opção mais rápida da lista, e é a única indisponível para nós. |
| Otimista com retry limitado | Certo quando a contenção é rara, e a contenção aqui é o cenário avaliado: o teste obrigatório disputa duas apostas pelo mesmo saldo e a suíte manda a mesma aposta cinquenta vezes. Um laço de retry ali é tempestade de retry mais um teto de tentativas que vira outro modo de falha. Continua usado onde a contenção é rara: publisher de outbox e worker de referências. |

**Granularidade.** O enunciado proíbe lock global, então a granularidade é a carteira. Um advisory lock
por carteira serializa o mesmo trabalho, mas não tem vínculo com a linha: um caminho futuro que
esqueça de tomá-lo perde a garantia em silêncio. O lock de linha não pode ser esquecido por nada
que leia a linha.

**A espera é limitada, não recusada.** `DB_LOCK_TIMEOUT` (padrão `3s`) vai como `lock_timeout` em
toda conexão do pool; carteira disputada além disso responde `503` com `Retry-After` em vez de
segurar uma conexão até o cliente desistir. `NOWAIT` foi descartado porque falha em qualquer
contenção, o que transformaria 49 das 50 submissões paralelas em `503`. Custo: o padrão foi calibrado
contra os testes deste repositório, não contra tráfego real.

**O que a garantia não pode repousar sobre.** Mutex em processo, ator por carteira e
`MessageGroupId = walletId` reduzem contenção e nenhum é a garantia — os invariantes devem valer
independentemente de locks locais e da deduplicação FIFO, e o caminho HTTP nunca toca a fila.
`test/system` roda os cenários contra três processos independentes.

## 3. Fronteira transacional

Um caso de uso nunca recebe, abre ou commita transação. `WalletRepository.Open` e
`WagerRepository.Process` são cada um um commit inteiro; o caso de uso passa agregados e, no
`Process`, um callback `Decide` executado dentro da transação contra a carteira lida sob o lock.

| Caso de uso | O que entra no mesmo commit |
| --- | --- |
| Abertura de carteira | carteira; e, se o saldo inicial > 0, a `OPENING` `PROCESSED`, o lançamento de crédito e os dois eventos de outbox |
| Submissão de operação | linha de inbox (quando veio da fila), `wager_transaction`, mutação da carteira, lançamento, estado final e eventos |
| Retomada de referência | a mesma decisão, sob o mesmo lock de carteira, numa transação por registro |
| Reconciliação | só leitura, em um `SELECT` |

Isso torna irrepresentável: mudança de saldo sem lançamento; evento sem a mudança que o causou;
linha de inbox separada do efeito de domínio; publicação antes do commit, já que
quem publica é um worker sobre linhas já commitadas. Custo: um caso de uso que precisasse de dois
agregados num commit precisa de um novo método de repositório, não de composição.

**A abertura credita ao registrar, não ao aplicar (A.5).** `walletapp.Open` constrói a carteira já
com o saldo inicial e monta o lançamento de crédito diretamente. O enunciado fixa a versão da carteira
recém-aberta em `1`, e a versão só incrementa quando o saldo muda — creditar uma carteira zerada
deixaria versão `2`. O lançamento continua validado, único e append-only. Custo: essa é a única
chamada a `NewLedgerEntry` fora da reidratação; uma segunda é falha de revisão, não precedente.

## 4. Idempotência

É persistente: chave, hash e resultado são colunas de `wager_transactions`, e a unicidade é índice. Nenhuma instância guarda identidade de submissão em memória, então qualquer instância
responde um replay igual, inclusive na primeira requisição depois de um restart.

**O hash.** SHA-256 sobre JSON canônico, em hexadecimal. As chaves saem ordenadas porque
`encoding/json` ordena as de um mapa, então os bytes dependem só dos valores.

- **Entram:** `providerId`, `externalTransactionId`, `playerId`, `walletId`, `roundId`, `gameId`,
 `kind`, `amountMinor`, `currency`, `referenceExternalTransactionId`.
- **Ficam de fora:** a chave de idempotência e todo metadado de transporte — cabeçalhos HTTP, o
 envelope SQS e seu `messageId`.

O enunciado exige que HTTP e SQS compartilhem o caso de uso, e o digest é onde isso fica visível: os dois
montam o mesmo `SubmitParams` e chamam o mesmo `Submit`, o decodificador da fila passa o `money`
pelo mesmo codec do corpo HTTP, e o `messageId` cai num campo que o hash ignora.

| Situação | Índice | Status | Corpo | `idempotentReplay` |
| --- | --- | --- | --- | --- |
| Mesma chave, conteúdo equivalente | `…_provider_key_unique` | `200`, ou `202` numa espera | o resultado persistido | `true` |
| Mesma chave, conteúdo diferente | `…_provider_key_unique` | `409` | `IDEMPOTENCY_KEY_CONFLICT` | — |
| Mesmo `(providerId, externalTransactionId)` sob outra chave | `…_provider_external_unique` | `409` | `EXTERNAL_TRANSACTION_CONFLICT` | — |

`Submit` descobre qual índice disparou relendo pela chave: registro cujo hash difere é conflito de
payload; nenhum registro sob a chave significa que o outro índice falou. O cabeçalho
`Idempotency-Key` é obrigatório e guardado como recebido — o cliente pode montá-lo como
`{providerId}:{externalTransactionId}`, mas o servidor nunca troca o que recebeu por um valor
calculado.

**Não há leitura prévia:** toda submissão tenta o `INSERT` e a violação de unicidade é a checagem.
`SELECT`-e-depois-`INSERT` perde a corrida — duas submissões da mesma chave em dois processos veem
nada e inserem as duas. Custo: uma duplicata custa uma transação revertida e uma releitura.

**O replay devolve o saldo do processamento original.** `MarkProcessed` grava o saldo daquele
commit em `result_balance_minor`, e o replay renderiza a coluna em vez de ler a carteira, como o enunciado
exige. Custo: é um snapshot — para saldo corrente existe `GET /wallets/:walletId`.

## 5. Máquina de estados e códigos de falha

| De | Pode ir para |
| --- | --- |
| `PENDING` | `PENDING_REFERENCE`, `PROCESSED`, `REJECTED`, `FAILED` |
| `PENDING_REFERENCE` | `PROCESSED`, `REJECTED`, `FAILED` |
| `PROCESSED`, `REJECTED`, `FAILED` | nada |

**`PENDING` não tem aresta de entrada.** Nada aponta para ele depois da construção, e é isso que
torna verificável a promessa de que todo `PENDING` commitado é retomável: uma linha assim é
sempre a que uma instância interrompida escreveu, nunca um retrocesso. Quem retoma é
`internal/worker/reference`, que roda em toda instância. Um registro terminal nunca transiciona de
novo; um replay o lê e devolve o saldo registrado.

**Transitória versus permanente.** Permanente é a falha que nenhuma reentrega muda: recusa de
construtor, tipo não suportado e os três conflitos de identidade — o consumidor manda para a DLQ na
hora. O resto espera o visibility timeout, com a redrive policy de rede de segurança. Uma chamada
síncrona não tem orçamento de retentativa, então o HTTP responde `503` com `Retry-After` e nada
fica commitado. **Limitação:** `Fail` existe e `FAILED` renderiza como `500`, mas nenhum caminho de
produção o chama — a falha permanente de infraestrutura é dead-letterada, então o estado de
auditoria não é alcançado.

| Código | Significado | Classe |
| --- | --- | --- |
| `WALLET_NOT_FOUND` | carteira inexistente, ou não é deste jogador | corrigível |
| `WALLET_CURRENCY_MISMATCH` | moeda do movimento ≠ moeda da carteira | corrigível |
| `REFERENCE_MISMATCH` | a referência discorda em jogador, carteira, rodada ou moeda, ou o par de tipos não é uma reversão válida | corrigível |
| `REFERENCE_AMOUNT_MISMATCH` | valor da reversão ≠ valor referenciado | corrigível |
| `INSUFFICIENT_FUNDS` | `BET` que o saldo não cobre; o ramo do `WIN` carrega o mesmo código como fallback, inalcançável por um crédito | definitivo |
| `REVERSAL_EXCEEDS_BALANCE` | o débito da reversão excede o saldo | definitivo |
| `REFERENCE_NOT_FOUND` | a espera passou de `reference_deadline_at` | definitivo |
| `REFERENCE_NOT_PROCESSED` | a referência é terminal e não é `PROCESSED` | definitivo |
| `REFERENCE_ALREADY_REVERSED` | a referência já carrega uma reversão bem-sucedida | definitivo |
| `INTERNAL_ERROR` | falha permanente de infraestrutura registrada para auditoria | definitivo |

**A regra de pertencimento.** Um código só existe aqui onde `Reject` ou `Fail` conseguem chegar —
isto é, onde a linha já existe. Tudo que um construtor recusa (um `LOSS` diferente de zero, uma
`OPENING` vinda de fora) não vira registro e portanto não tem `FailureCode`: é reportado pelo corpo
RFC 9457, cujas constantes vivem em `internal/adapter/httpapi/problem.go` (A.3.5). **Um `code` num
corpo de erro não é necessariamente um `FailureCode`** — `VALIDATION_FAILED` e
`SERVICE_UNAVAILABLE` são vocabulário de transporte.

Carteira inexistente e carteira de outro jogador compartilham `WALLET_NOT_FOUND` de propósito:
separá-los entregaria ao provedor um oráculo de enumeração (A.7). Custo: quem chamou não
distingue erro de digitação de fronteira de permissão. `INTERNAL_ERROR` é o único código de
`FAILED`; um segundo só serviria para separar mensagem envenenada de dependência morta.

## 6. Os cinco tipos, referências e reversões

| Tipo | Movimento | Valor | Referência | Ledger | Versão da carteira |
| --- | --- | --- | --- | --- | --- |
| `BET` | débito | > 0 | recusada | um `DEBIT` | +1 |
| `WIN` | crédito | > 0 | opcional, só gravada | um `CREDIT` | +1 |
| `LOSS` | nenhum | = 0 | recusada | nenhum | inalterada |
| `REFUND` | crédito | > 0 | obrigatória, resolvida | um `CREDIT` | +1 |
| `ROLLBACK` | o oposto do original | > 0 | obrigatória, resolvida | um lançamento | +1 |
| `OPENING` | crédito de abertura | ≥ 0 | não se aplica | um `CREDIT`, se > 0 | fica em `1` (A.5) |

Carteira e moeda são conferidas antes do despacho por tipo, então um `LOSS` na moeda errada é
recusado com `WALLET_CURRENCY_MISMATCH` embora não mova nada. O zero do `LOSS` é testado por
**valor**, não contra o literal `"0.00"`: `"0"` e `"0.0"` são o mesmo valor depois de A.3.1, e
comparar texto recusaria um `LOSS` válido de quem omite os decimais. Um `LOSS` processado ainda
responde um saldo — o inalterado — porque a regra de replay vale para ele como para
qualquer outro desfecho.

| Tipo | `referenceExternalTransactionId` | Resolvido? |
| --- | --- | --- |
| `REFUND`, `ROLLBACK` | obrigatório | sim, por `(providerId, reference…)` |
| `WIN` | opcional: pode citar uma aposta da rodada | não: só guardado e hasheado |
| `BET`, `LOSS` | recusado, `400` | — |

A referência de um `WIN` é gravada e entra no hash, mas nenhuma busca e nenhuma checagem de acordo
rodam (A.4): O enunciado prende resolução e acordo às duas reversões, e resolver a de um `WIN` deixaria um
crédito `PENDING_REFERENCE` esperando uma aposta que o próprio enunciado chama de opcional. Se essa
leitura estiver errada, é um ramo antes do crédito. Recusar o campo em `BET`/`LOSS` é ler silêncio
como proibição (A.4); custo: um cliente que sempre preenche o campo leva `400`.

| Reversão | Referência | Movimento |
| --- | --- | --- |
| `REFUND` | `BET` | crédito |
| `ROLLBACK` | `BET` | crédito |
| `ROLLBACK` | `WIN` | débito |
| `ROLLBACK` | `REFUND` | débito |

Qualquer outro par — um `REFUND` de um `WIN`, um `ROLLBACK` de um `LOSS` ou de outra reversão —
está fora da tabela acima e é recusado com `REFERENCE_MISMATCH`.

**Acordo.** Jogador, carteira, rodada e moeda são conferidos; o provedor é a chave da busca, e a
igualdade de valor é `Money.Cmp` sobre unidades mínimas. Tudo roda **antes** do movimento, dentro da
transação que segura o lock da carteira, então uma reversão recusada deixa o saldo intocado. A
linha da referência não toma lock próprio: a coordenação fica na carteira, e uma reversão só
chega a `PROCESSED` depois de concordar com a referência sobre a carteira, então duas que poderiam
colidir já estão na fila daquele lock. O id interno resolvido é gravado em
`reference_transaction_id` inclusive numa reversão recusada — para onde ela apontou faz parte do
registro de auditoria.

**`REFUND` e `ROLLBACK` sobre a mesma aposta**, que o enunciado manda documentar: uma reversão bem-sucedida
por referência, **qualquer que seja o tipo** — uma aposta já estornada recusa o `ROLLBACK` e
vice-versa, com `REFERENCE_ALREADY_REVERSED`. Desfazer um estorno é um `ROLLBACK` *do `REFUND`*.
A migration `0009` impõe isso com índice único parcial sobre `reference_transaction_id` onde a
linha é `REFUND` ou `ROLLBACK` `PROCESSED`; chavear por `(referência, tipo)` satisfaria só a frase
mais fraca da regra e devolveria o mesmo débito duas vezes (A.8.1). Custo: a segunda reversão de uma
cadeia precisa citar a anterior.

| Operação | Referência | Movimento | Saldo, partindo de 100.00 com aposta de 25.00 |
| --- | --- | --- | --- |
| `BET` | — | débito 25.00 | 75.00 |
| `REFUND` | a `BET` | crédito 25.00 | 100.00 |
| `ROLLBACK` | o `REFUND` | débito 25.00 | 75.00 |

| Estado da referência | Resultado |
| --- | --- |
| ausente | `PENDING_REFERENCE`, `202`, retentada até o TTL |
| `PENDING` / `PENDING_REFERENCE` | o mesmo — ainda não disponível, e o enunciado garante que vai assentar |
| `PROCESSED` | resolvida; valem acordo, valor e reversão única |
| `REJECTED` / `FAILED` | `REJECTED` na hora, com `REFERENCE_NOT_PROCESSED` |

Rejeitar de imediato uma referência `PENDING` é a outra resposta defensável à pergunta do enunciado
(A.8.2): esperar custa um atraso a um pedido corrigível; rejeitar custa a ele uma recusa evitável.

**A espera.** O prazo é carimbado uma vez, quando a espera é registrada, a partir de
`REFERENCE_TTL` (padrão `24h`) — assim uma mudança de política não expira retroativamente um
registro. O worker varre a cada `REFERENCE_POLL_INTERVAL` (padrão `1s`) as linhas cujo
`next_attempt_at` chegou, uma transação SQL por registro sob o lock da carteira; cada tentativa
agenda `now() + least(1 << least(attempts, 8), 300)` segundos — dobra, com teto de cinco minutos, em
aritmética inteira porque o enunciado proíbe ponto flutuante. As duas colunas são persistidas, então um
restart retoma a espera. Vencido o prazo, o registro termina `REJECTED` com `REFERENCE_NOT_FOUND`.

Uma reversão que sacaria mais que o saldo recebe código diferente do de uma aposta sem fundos, como
como exigido: `REVERSAL_EXCEEDS_BALANCE` contra `INSUFFICIENT_FUNDS`. É auditável — a linha commitada
guarda o código, a referência resolvida e o saldo, além de emitir `WagerTransactionRejected`.

## 7. Inbox e outbox

**Inbox.** A identidade durável é o `messageId` do envelope, única por `(consumerName, messageId)`. `consumer_name` é a constante `"wager-transactions"`, não configuração: dois deploys
com valores diferentes desligariam a deduplicação em silêncio. A linha de inbox é inserida na mesma
transação da mudança de domínio, e a mensagem só é apagada depois desse commit — uma queda no meio é
replay, não perda: o rollback não deixa linha alegando tratamento, e o commit faz a reentrega bater
na chave primária e ser respondida a partir do armazenamento. Um `PENDING_REFERENCE` é tratado e
apagado, como o enunciado permite, com o worker assumindo a espera.

Uma reentrega com hash diferente para o mesmo `messageId` é recusada e dead-letterada: apagá-la
descartaria uma mensagem que o produtor acha que enviou, e tratá-la deixaria um `messageId` cobrir
duas operações. O enunciado exige a checagem, não diz o que fazer — a DLQ é onde fica o que ninguém pode
tratar.

**Outbox.** `insertOutbox` roda dentro da transação do chamador e nada no caminho da requisição
envia, então a regra de publicar só depois do commit vale por construção, ao custo de um `OUTBOX_POLL_INTERVAL` (`1s`) de latência. Um
ciclo é uma transação: reivindicar as linhas devidas com `FOR UPDATE SKIP LOCKED`, enviar cada uma,
registrar o desfecho, commitar. Linha que outro publisher segura é invisível, então toda instância
drena a tabela sem lock global, e as linhas de um publisher morto são liberadas pelo próprio
Postgres — sem coluna de lease, sem varredor.

**A transação é o lease.** Não há par `claimed_by`/`claimed_at` nem timeout a esperar: um publisher
que morre tem a conexão fechada pelo Postgres, a transação revertida e as linhas visíveis ao próximo
imediatamente. Um publisher que *trava* em vez de morrer é limitado pelo `OUTBOX_PUBLISH_WINDOW`
(`10s`), o prazo do ciclo inteiro — esse é o teto de quanto tempo uma linha reivindicada fica presa.

| Queda entre | O que sobrevive | O que o ciclo seguinte faz |
| --- | --- | --- |
| o commit e o envio | a linha, com `published_at IS NULL` | reivindica e envia |
| o envio e a confirmação | a linha, ainda não publicada | envia de novo, byte a byte igual |

O segundo caso é por que o worker nunca remonta o payload: ele encaminha a coluna como foi escrita,
de modo que a republicação leva o mesmo `eventId`, que é o `MessageDeduplicationId` da fila.
Uma repetição dentro da janela de deduplicação é descartada pelo SQS, mas a janela é finita — o
consumidor precisa ser idempotente de qualquer forma.

Um envio que falha commita `attempts + 1` e o mesmo backoff das referências (um segundo dobrando até cinco
minutos), porque um rollback perderia o backoff. Não há limite de tentativas nem DLQ aqui: perder um evento cujo registro foi commitado é
proibido, então uma linha impublicável retenta para sempre,
visível em `outbox_lag_seconds`. A coluna `payload` é encaminhada como foi escrita, então uma
republicação mantém o `eventId`.

| Evento | Gatilho |
| --- | --- |
| `WagerTransactionProcessed` | operação concluída com sucesso, `LOSS` incluído |
| `WagerTransactionRejected` | recusa definitiva por regra de negócio; carrega `failureCode` |
| `WalletBalanceChanged` | mudança efetiva de saldo; ausente em `LOSS` e em abertura com saldo zero |
| `WagerTransactionPendingReference` | registro da espera por uma referência |

O envelope carrega `eventId`, `eventType`, `aggregateId`, `correlationId`, `causationId` opcional,
`occurredAt`, `version` e `data` tipado. `eventType` e `version` são definidos pelos construtores,
nunca recebidos, então nenhum chamador rotula um evento errado; `aggregateId` é a carteira mesmo
nos eventos de transação. `occurredAt` é RFC 3339 em UTC, dinheiro sai como string decimal, e o
payload é um snapshot — uma transição posterior não altera o que já foi escrito.
`WalletBalanceChanged` leva `walletId`, `transactionId`, `direction`, `money`, `balanceBefore`,
`balanceAfter` e `walletVersion`. **Limitações:** `causationId` nunca é preenchido e `version` é uma
constante.

## 8. Contratos das filas

| Fila | `MessageGroupId` | `MessageDeduplicationId` | Por quê |
| --- | --- | --- | --- |
| `wager-transactions.fifo` (entrada) | a carteira, definida pelo produtor | do produtor, por mensagem | uma carteira mantém ordem, carteiras diferentes correm em paralelo, e uma mensagem travada só bloqueia o próprio grupo |
| `wager-transactions-dlq.fifo` | o grupo original, ou o `messageId` na falta dele | o `messageId` | uma DLQ FIFO exige grupo; reagrupar mantém a falha junto da carteira |
| `wager-events.fifo` (saída) | `aggregateId`, a carteira | o `eventId` | uma republicação é a mesma mensagem, não uma segunda |

As três são FIFO com `ContentBasedDeduplication` desligado. `VisibilityTimeout` é `30` segundos —
longo o bastante para um tratamento, curto o bastante para a mensagem de um consumidor morto voltar
depressa — e `maxReceiveCount` é `3` na redrive policy que aponta para a DLQ. Uma fila de saída só
carrega os quatro tipos e o consumidor roteia por `eventType`; uma fila por tipo colocaria
`WagerTransactionProcessed` e `WalletBalanceChanged` — um mesmo commit — em destinos cuja ordem não
se compara. A ordem é por carteira apenas, a entrega é at-least-once, e nenhuma garantia repousa
sobre a deduplicação FIFO: a janela do SQS é de cinco minutos e só alcança
`MessageDeduplicationId` idênticos, enquanto a inbox e os dois índices de idempotência valem entre
reinícios e entre transportes. Os testes de integração provam isso enviando o mesmo corpo duas vezes
sob ids de deduplicação **diferentes**, de modo que a fila não deduplica e a aplicação precisa
deduplicar.

| Desfecho | Causa | Mensagem |
| --- | --- | --- |
| tratada | `PROCESSED`, `REJECTED`, `PENDING_REFERENCE`, replay idempotente | **apagada**, depois do commit |
| recusada em definitivo | recusa de construtor, tipo não suportado, os três conflitos de identidade | **enviada à DLQ pelo próprio consumidor e apagada**, na hora |
| transitória | carteira ocupada, banco ou fila inacessível | **intocada**; o visibility timeout reentrega e a redrive policy é a rede de segurança |

O consumidor dead-lettera a falha permanente em vez de esperar `maxReceiveCount` porque a fila é
FIFO: três tentativas condenadas seguram o grupo daquela carteira por noventa segundos. Se esse
envio falhar, a mensagem fica para a redrive policy. Corpo indecifrável, `messageId` ou
`idempotencyKey` ausente, id impossível de parsear e tudo que o construtor recusa seguem esse
caminho e não o de rejeição: nunca tendo sido transação, não há o que marcar `REJECTED` nem evento
a emitir.

**`SIGTERM`.** O consumidor guarda dois contextos. O primeiro é cancelado quando o desligamento
começa, abortando o long poll para que nenhum trabalho novo entre; o segundo mantém vivo o
tratamento em voo até `WORKER_DRAIN_TIMEOUT` (`5s`), e cancelá-lo reverte a transação e deixa a
visibilidade da mensagem expirar para uma reentrega segura.

## 9. Autenticação e autorização

**Keycloak com `client_credentials`.** O enunciado exige IdP OIDC externo e recomenda Keycloak; quem chama
são serviços, não pessoas, então é o único grant provisionado, e `keycloak/realm.json` sobe junto do
Compose. Custo: o realm roda `start-dev` sobre HTTP puro — postura local, não implantável.

Este serviço é **resource server e nada mais**: o Keycloak assina, nós verificamos. Não existe
segredo de assinatura deste lado — o realm publica a chave pública no JWKS — e não existe endpoint
de login, que o enunciado põe fora de escopo. O realm é dado versionado: `wagering-api` é `bearerOnly` e só
figura como audiência, um mapper carimba `aud` nos clients que devem alcançá-la, e `provider_id` é
um mapper por client, nunca um campo que o chamador preenche.

**O token é verificado, nunca só decodificado.** Assinatura contra o JWKS do issuer, `iss` contra
`OIDC_ISSUER_URL`, `aud` contra `OIDC_AUDIENCE`, `exp`/`nbf`, mais duas checagens que a biblioteca
deixa para o chamador: `RS256` é fixado (vazio, a lista herdaria os treze algoritmos que o Keycloak
anuncia, HMAC inclusive) e `typ` precisa ser `Bearer`, de modo que um ID token não autoriza chamada.
O discovery roda num hook de start — é chamada de rede a um IdP que pode estar subindo, e no
construtor viraria grafo que não monta em vez de falha de partida legível; um verificador que não
descobriu nada recusa todo token. Custo: validação é sem estado, então um token revogado dentro da
validade é aceito até expirar.

**Dois endereços para um IdP.** O token pedido do host carrega
`iss: http://localhost:8081/realms/wagering`, que é o que `KC_HOSTNAME` fixa, mas o container não
alcança `localhost:8081`. Daí `OIDC_ISSUER_URL` ser o que o token precisa *alegar* e
`OIDC_DISCOVERY_URL` ser de onde as chaves são *buscadas* (`oidc.InsecureIssuerURLContext`, que
apesar do nome não desliga a checagem de issuer — aponta-a para o valor configurado). Fora do
Docker os dois são iguais.

**A decisão é por escopo, não por papel.** `wallets` é concedido só ao `internal-service` no realm,
então um token de provedor verifica bem e só então leva `403`. Escopo em vez de role mantém a
decisão num lugar só — o realm — e visível dentro do próprio token. O guard roda antes do handler,
e a recusa é opaca para quem chamou: o motivo real vai para o log, nunca para o corpo. `401` é "não
sei quem você é" (sem token, token ruim, expirado) e acompanha `WWW-Authenticate: Bearer`; `403` é
"sei quem você é e você não pode isto".

| Client (realm `wagering`) | Escopo | Alcança | `provider_id` |
| --- | --- | --- | --- |
| `internal-service` | `wallets` | `POST /wallets`, `GET /wallets/{id}`, `/ledger`, `/reconciliation` | — |
| `provider-a`, `provider-b` | `wagering` | `POST /wagering/transactions` e as duas leituras de transação | `provider-a` / `provider-b` |
| `provider-expiring` | `wagering` | o mesmo, com token de um segundo — existe para o teste de expiração | — |
| `outsider` | nenhum | nada; o token tem outra audiência, para o teste de audiência | — |
| *(sem autenticação)* | — | `GET /health/live`, `/health/ready`, `GET /metrics` | — |

**A claim do provedor é comparada antes de qualquer outra coisa ser reportada.** Um corpo cujo
`providerId` difere da claim leva `403` antes da validação de campos, para que ninguém sonde o
contrato de outro provedor. A transação de outro provedor, por id, responde `404` e não `403`,
que confirmaria a existência do id; em `/providers/{providerId}/...` responde `403`, porque aquele
id veio de quem chamou. Replays também são escopados: a busca é por `(providerId, chave)`. Um
pedido recusado nunca chega ao caso de uso, então não move dinheiro nem expõe dado.

**Interpretação — a fila de entrada é ingresso confiável.** O enunciado manda uma única
`wager-transactions.fifo` e o SQS não dá ao consumidor identidade de remetente, então a regra de
identidade do chamador não se aplica ao pé da letra ali: `data.providerId` é autoritativo e a política de
acesso da fila é o único portão. A validação de domínio não muda — o consumidor chama o mesmo
`Submit`. Uma postura mais estrita custaria uma fila por provedor, contrariando o enunciado, ou uma
assinatura de payload que o enunciado nunca menciona.

**Interpretação — as políticas do broker são declaradas, não provadas.**
As credenciais vêm de `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` — `test`/`test` contra o
LocalStack, credenciais IAM reais em qualquer outro lugar — e `localstack/init-queues.sh` anexa a
cada fila a política com a ação mais estreita que cada principal precisa:

| Fila | Principal | Ação |
| --- | --- | --- |
| `wager-transactions.fifo` | `wagering-producer` | `sqs:SendMessage` |
| `wager-transactions-dlq.fifo` | `wagering-api` | `sqs:SendMessage`, `sqs:ReceiveMessage` |
| `wager-events.fifo` | `wagering-api` | `sqs:SendMessage` |

Esses documentos implantam contra SQS real sem mudança. O que o LocalStack faz com eles foi **medido, não suposto**:
com `ENFORCE_IAM=1`, um usuário IAM sem política nenhuma completou `sqs:SendMessage` na edição
community 4. Por isso nenhum teste afirma que um principal não autorizado é recusado — passaria
aqui pelo motivo errado. Isso decorre da escolha de emulador (A.2), não do enunciado.

## 10. Contratos HTTP

Respostas não-2xx são `application/problem+json` (RFC 9457) com `type`, `title`, `status`, `code`,
`detail` e os membros de extensão `instance`, `idempotentReplay` e `errors`. A validação acumula
todas as violações em `errors[]`, para que o cliente conserte o pedido numa ida só — isso é escolha,
não exigência: O enunciado só pede que as classes sejam distinguíveis (A.3.5).

| Classe | Status | Corpo | Quando |
| --- | --- | --- | --- |
| Sucesso | `200`, `201` | o recurso, ou `{transactionId, status, balance, idempotentReplay}` | leituras, carteira aberta, operação `PROCESSED` — e seu replay, com o saldo do processamento original |
| Processamento pendente | `202` | o mesmo, sem saldo | reversão registrada como `PENDING_REFERENCE` |
| Entrada inválida | `400` | `VALIDATION_FAILED` com `errors[]`, ou `MALFORMED_BODY` | JSON impossível de parsear, campo ausente, `Idempotency-Key` ausente, tipo inválido, cursor ou limite ruins |
| Não autenticado / não autorizado | `401` + `WWW-Authenticate`, `403` | `UNAUTHENTICATED`, `FORBIDDEN` | sem token ou com token que o JWKS não valida; escopo ausente ou `providerId` não autorizado |
| Não encontrado | `404` | `WALLET_NOT_FOUND`, `TRANSACTION_NOT_FOUND` | carteira desconhecida numa leitura; transação de outro provedor |
| Conflito | `409` | `WALLET_ALREADY_EXISTS`, `IDEMPOTENCY_KEY_CONFLICT`, `EXTERNAL_TRANSACTION_CONFLICT` | segunda carteira para jogador e moeda; chave reusada com outro conteúdo; operação reenviada sob segunda chave |
| Rejeição de negócio | `422` | `code` = `failureCode`, `instance` = o caminho da transação | recusa decidida contra uma transação persistida; o replay é o mesmo `422` |
| Falha permanente registrada | `500` | `code` = `failureCode` | uma transação `FAILED` relida |
| Indisponibilidade transitória | `503` + `Retry-After` | `SERVICE_UNAVAILABLE` | carteira disputada além do `DB_LOCK_TIMEOUT`, ou dependência inacessível — nada aplicado |

A separação entre `503` e `500` pergunta ao erro se ele é `SafeToRetry()`, coisa que o pgx só marca
para falha que nunca chegou ao servidor. Custo: uma falha que chegou ao servidor mas não commitou
nada aparece como `500`.

**Paginação do ledger.** O cursor é o `(created_at, id)` do último lançamento, em base64url,
casando com o `ORDER BY` da consulta. Um offset se desloca quando um insert concorrente cai no meio
da página; um cursor de chave sobre tabela append-only não. A codificação o torna opaco como pedido, não secreto — cursor forjado é `400`. O limite é 50 por padrão e no máximo 200, definido no
caso de uso, então todo transporte pagina igual. Custo: sem contagem de páginas e sem pular para a
página N.

## 11. Fx, ciclo de vida e shutdown

Um `fx.Module` por preocupação, ligados em `cmd/api/main.go`. Handlers não conhecem o mux: cada um
contribui uma rota por value group. Duas portas são amarradas só no `main` — o `Resolver` do worker
de referências e o `Submitter` do consumidor, ambos satisfeitos pelo serviço de wagering — para que
a camada de aplicação não importe worker nenhum e nenhum worker importe a aplicação.

**A partida falha no boot ou não falha.** A configuração acumula todos os problemas e reporta numa
passada só, então um deploy a que faltam três variáveis descobre as três; o pool é pingado sob
`STARTUP_TIMEOUT`, porque o pgx conecta preguiçosamente e um banco inacessível apareceria só na
primeira requisição; o listener é aberto dentro do `OnStart`, então porta ocupada é falha de
partida.

**O shutdown roda os `OnStop` em ordem inversa de registro:** os workers param de buscar trabalho
antes de o servidor HTTP parar de aceitar, e o pool fecha depois de tudo que o usa. Cada worker pede
ao laço que pare, deixa o ciclo em voo terminar e só cancela como último recurso — um tratamento
abandonado é revertido, não aplicado pela metade.

**`WORKER_DRAIN_TIMEOUT` (5s) é a cota de cada worker, não o `SHUTDOWN_TIMEOUT` (15s).** O enunciado pede que
o trabalho em voo termine *e* que o servidor encerre graciosamente; num orçamento só, um worker
lento gastaria tudo e o servidor, que para depois dele, não receberia nada. O prazo de drenagem
deriva do contexto do shutdown, então a espera efetiva é a menor das duas e um valor maior
simplesmente não tem efeito — o par não precisa de validação cruzada.

**O domínio não importa Fx, HTTP, SQS nem pgx.** A direção mantém isso verdadeiro: a aplicação
declara as portas e o adapter as satisfaz. Custo: diferente da proibição de float, que
`TestInternalContainsNoFloat` garante caminhando na AST, esta regra não tem teste — um import novo
é pego em revisão, não pelo `go test`.

## 12. Observabilidade e reconciliação

**Correlation id.** Um UUID por operação, carregado no contexto. O HTTP adota o `X-Correlation-Id`
quando ele é um UUID, gera um caso contrário, e ecoa o que usou; o consumidor adota o
`correlationId` do envelope; cada ciclo de worker gera o seu. O handler de log é embrulhado, então
toda chamada com contexto sai carimbada sem que o call site precise lembrar, e o mesmo id vai para
os envelopes de outbox, caindo para o id da própria transação quando não há um. `messageId`,
`walletId`, `transactionId` e `providerId` continuam explícitos.

**O que nunca é logado:** tokens, segredos de client e payload financeiro completo. Um token
recusado loga o motivo, não o token; uma operação submetida loga identificadores e status, não o
valor; uma falha de readiness nomeia a dependência, não o erro. A exceção é a divergência de
reconciliação, onde os saldos são o achado. `TestLogsCarryTheIdentifiersAndNoCredentials` captura o
stdout do processo e falha se uma credencial ou um valor aparecer.

| Métrica | Responde |
| --- | --- |
| `wager_outcomes` | quantas operações terminaram `PROCESSED`, `REJECTED`, `PENDING_REFERENCE` |
| `wager_duplicates` | com que frequência a idempotência devolveu resultado armazenado |
| `wallet_concurrency_conflicts` | com que frequência uma carteira foi disputada além do lock timeout |
| `reference_retries` | quanto trabalho a varredura de referências carrega |
| `sqs_dead_lettered` | quantas mensagens o consumidor recusou em definitivo |
| `reconciliation_divergences` | com que frequência um saldo não bateu com o ledger |
| `wager_processing` | `count` e `total_ms` — a latência média de submissão |
| `outbox_lag_seconds` | a idade do evento não publicado mais antigo |

A contagem acontece em `Submit`, o ponto que os dois transportes compartilham, exceto
dead-lettering (consumidor), retentativas (varredura), divergências (handler de reconciliação) e o
atraso — uma `expvar.Func` que roda `MIN(occurred_at)` na hora do scrape, porque um contador mantido
pelo publisher leria zero justamente quando o publisher é o que parou. Custo: sem histograma, sem
percentis; o upgrade é um cliente Prometheus na mesma rota.

**Health checks.** `GET /health/live` promete só que o processo atende e continua `200` com
dependência degradada, para que um banco travado não reinicie uma instância sadia.
`GET /health/ready` pinga o pool e chama `GetQueueAttributes` na fila de entrada sob um teto fixo de
2s, respondendo `503` com o estado de cada dependência. Ambos públicos, como `/metrics`: os corpos
não nomeiam jogador, carteira nem valor.

**Reconciliação.** `POST /wallets/{walletId}/reconciliation` reconstrói o saldo a partir do ledger,
abertura incluída, e reporta `difference` como o saldo armazenado menos o reconstruído. Não
escreve nada, e os dois valores vêm de um único `SELECT` que junta a carteira aos seus lançamentos:
o Postgres tira um snapshot por instrução, então um movimento que commite no meio da leitura não
pode aparecer de um lado só — uma transação repeatable read não compraria nada. Uma divergência cai
onde deve cair: no corpo, no log e na métrica.

## 13. Verificação

| Suíte | Comando | O que cobre |
| --- | --- | --- |
| Unitária | `go test -race./...` | `Money`, invariantes da carteira, transições, as regras dos cinco tipos externos, conflito de payload, política de valor zero, abertura interna com seus eventos |
| Integração | `go test -race -tags=integration./...` | Postgres, Keycloak e LocalStack reais: migrations, constraints, imutabilidade do ledger, atomicidade, inbox, reentrega, DLQ, outbox concorrente, recuperação, composição Fx com start e stop |
| Multi-instância | `go test -race -tags 'integration,system'./test/system/...` | **três processos independentes** contra containers compartilhados |

A suíte de sistema compila o binário com o detector de corrida e sobe três processos, cada um com
suas conexões e memória. Ela cobre: a mesma aposta cinquenta vezes com o valor escrito de três
formas, provando que a normalização chega ao hash nos dois transportes; as duas apostas de 80.00
contra 100.00, com reenvio; uma carteira travada que não bloqueia outra — a sobreposição é
observada, não presumida; um consumidor morto com `SIGKILL` e todas as mensagens reenviadas; uma
referência pendente retomada por uma instância sobrevivente; três publishers drenando uma outbox com
um deles morrendo. Todo cenário termina reconciliando todas as carteiras que tocou.

## 14. Interpretações adotadas

A entrega exige que estejam explícitas. Cada uma é argumentada na seção indicada.

| # | Leitura adotada | Seção |
| --- | --- | --- |
| A.1 | a identidade da `OPENING` é um UUIDv7 comum, não derivado da carteira; um índice único parcial impede a segunda | Invariantes |
| A.2 | LocalStack é o emulador de SQS | Stack |
| A.3.1–A.3.3 | `"25"` e `"25.00"` são o mesmo valor, hasheado em unidades mínimas; a moeda é um registro fechado; negativos são recusados na fronteira, produzidos internamente e renderizados na saída | 1, 4 |
| A.3.5 | a validação mora no tipo, para os dois transportes compartilharem; as violações são reportadas juntas | 10 |
| A.4 | referência obrigatória nas reversões, opcional no `WIN` e guardada sem resolução, recusada em `BET` e `LOSS` | 6 |
| A.5 | o crédito de abertura é registrado, não aplicado, então a versão é `1` | 3 |
| A.6 | a coordenação é lock pessimista de linha com predicado de versão atrás | 2 |
| A.7 | aposta contra carteira inexistente é gravada `REJECTED` e respondida `422`, não `404` | Invariantes, 5 |
| A.8 | uma reversão bem-sucedida por referência, qualquer que seja o tipo; referência ausente ou inacabada é esperada, terminal sem sucesso é rejeitada na hora | 6 |
| Fila de entrada | o `providerId` da fila de entrada é autoritativo, com a política da fila como portão; essas políticas são anexadas mas não impostas pelo LocalStack | 9 |

## 15. Limitações e trabalho não concluído

- A escala é de duas casas para toda moeda; JPY e KWD exigiriam expoente por moeda.
- Só três moedas liquidam — BRL, EUR, USD. Qualquer outro código ISO 4217 é recusado.
- O LocalStack guarda as políticas de acesso das filas sem impô-las, então o menor privilégio é
 implantável mas não demonstrável localmente.
- Sem percentis de latência: `expvar` não tem histograma, só contagem e total.
- Sem tracing distribuído e sem teste de carga — os diferenciais opcionais não foram feitos.
- Uma topologia só: um binário roda a API, o publisher, o worker de referências e o consumidor, então
 escalar um escala os quatro.
- A reconciliação é sob demanda; nada reconcilia em agenda.
- `FAILED` não é alcançável em produção: a falha permanente é dead-letterada em vez de gravada.

**Atalhos deliberados**, marcados com `ponytail:` onde vivem.

| Marcador | Atalho | Upgrade |
| --- | --- | --- |
| `internal/domain/money/currency.go` | o registro de moedas é um conjunto, não uma tabela de metadados | `map[Currency]int` de expoentes, lido por `parseMinor` e `format` |
| `internal/platform/metrics/metrics.go` | latência é contagem e total | histograma Prometheus na mesma rota |
| A.6 (seção 2, acima) | o lock da carteira é tomado em todo movimento | o híbrido permitido: `UPDATE` atômico condicional e depois reidratar de `RETURNING`, deixando o agregado validar |

