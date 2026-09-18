//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"uuid"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/metrics"
	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
)

// message is the envelope, built around the same business fields the HTTP
// body carries so the two hash identically.
func message(messageID, externalID, playerID, walletID, kind, amount string) string {
	return fmt.Sprintf(`{"messageId":%q,"type":"WagerTransactionRequested",`+
		`"occurredAt":"2026-09-08T12:00:00.000Z","data":{`+
		`"providerId":"provider-a","externalTransactionId":%q,"idempotencyKey":"provider-a:%s",`+
		`"playerId":%q,"walletId":%q,"roundId":"round-987","gameId":"fortune-chimp",`+
		`"kind":%q,"money":{"amount":%q,"currency":"BRL"}}}`,
		messageID, externalID, externalID, playerID, walletID, kind, amount)
}

// send puts a message on the queue the consumer under test reads, grouped by
// wallet.
func (w *wagering) send(client *awssqs.Client, walletID, dedup, body string) {
	w.t.Helper()
	testsupport.Send(w.t, client, testsupport.InboundQueue(w.t), body, walletID, dedup)
}

// awaitBalance polls until the consumer has caught up, or gives up.
func (w *wagering) awaitBalance(walletID, want string) {
	w.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		got := w.balance(walletID)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("balance is %s after 30s, want %s", got, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// the same message delivered twice moves money once. FIFO
// deduplication is deliberately defeated with two deduplication ids, so what is
// proved is the application's inbox, not the queue's.
func TestARedeliveredMessageDebitsOnce(t *testing.T) {
	api := startWagering(t)
	client := testsupport.SQSClient(t, testsupport.SQSEndpoint(t))
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	body := message("msg-1", "transaction-123", playerID, walletID, "BET", "25.00")
	api.send(client, walletID, "first", body)
	api.awaitBalance(walletID, "75.00")

	replays := metrics.Duplicates.Value()
	api.send(client, walletID, "second", body)
	api.awaitReplayed(client, replays)

	if got := api.balance(walletID); got != "75.00" {
		t.Errorf("balance = %s, want one debit for two deliveries", got)
	}
	if got := api.debits(walletID); got != 1 {
		t.Errorf("ledger debits = %d, want 1", got)
	}
	if got := api.inbox(); got != 1 {
		t.Errorf("inbox rows = %d, want 1 for one messageId", got)
	}
	api.requireConsistent(walletID)
}

// HTTP and SQS share the use case, so one operation delivered over both
// produces one financial effect.
func TestTheSameOperationOverHTTPAndSQSAppliesOnce(t *testing.T) {
	api := startWagering(t)
	client := testsupport.SQSClient(t, testsupport.SQSEndpoint(t))
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	placed := api.bet(bet{
		externalID: "transaction-123", key: "provider-a:transaction-123",
		playerID: playerID, walletID: walletID, amount: "25.00",
	})
	if placed.Status != http.StatusOK {
		t.Fatalf("POST failed: %+v", placed)
	}

	// The queue spells the amount differently; A.3.1 makes it the same operation.
	replays := metrics.Duplicates.Value()
	api.send(client, walletID, "queued", message("msg-1", "transaction-123", playerID, walletID, "BET", "25"))
	api.awaitReplayed(client, replays)

	if got := api.balance(walletID); got != "75.00" {
		t.Errorf("balance = %s, want one debit across the two transports", got)
	}
	if got := api.debits(walletID); got != 1 {
		t.Errorf("ledger debits = %d, want 1", got)
	}
	api.requireConsistent(walletID)
}

// asks for the two inputs to be validated concurrently, not one after the
// other: the message and the posts race for the same wallet row and key.
func TestHTTPAndSQSRacingForOneOperationApplyItOnce(t *testing.T) {
	api := startWagering(t)
	client := testsupport.SQSClient(t, testsupport.SQSEndpoint(t))
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	const posts = 10
	replays := metrics.Duplicates.Value()
	api.send(client, walletID, "raced", message("msg-1", "transaction-123", playerID, walletID, "BET", "25.00"))

	results := make([]betResult, posts)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = api.bet(bet{
				externalID: "transaction-123", key: "provider-a:transaction-123",
				playerID: playerID, walletID: walletID, amount: "25.00",
			})
		}()
	}
	wg.Wait()
	api.awaitInboundQueueDrained(client)

	for _, result := range results {
		if result.Status != http.StatusOK {
			t.Fatalf("status = %d, want %d (%+v)", result.Status, http.StatusOK, result)
		}
	}
	// Eleven arrivals, one applied: every other one must be a proven replay.
	if got := metrics.Duplicates.Value() - replays; got != posts {
		t.Errorf("replays = %d, want %d", got, posts)
	}
	if got := api.debits(walletID); got != 1 {
		t.Errorf("ledger debits = %d, want 1", got)
	}
	if got := api.balance(walletID); got != "75.00" {
		t.Errorf("balance = %s, want 75.00", got)
	}
	api.requireConsistent(walletID)
}

// The brief separates the two failure kinds. A message the domain refuses can never
// succeed, so it goes to the dead-letter queue at once rather than holding its
// message group for three retries that cannot change the answer.
func TestAnUnhandleableMessageIsDeadLetteredAtOnce(t *testing.T) {
	api := startWagering(t)
	client := testsupport.SQSClient(t, testsupport.SQSEndpoint(t))
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	refused := []struct{ dedup, body string }{
		// OPENING is refused whatever transport carries it.
		{"opening", message("msg-opening", "transaction-opening", playerID, walletID, "OPENING", "25.00")},
		// A non-zero LOSS never becomes a record either.
		{"loss", message("msg-loss", "transaction-loss", playerID, walletID, "LOSS", "25.00")},
		{"broken", `{"messageId":"msg-broken","data":{`},
	}
	for _, m := range refused {
		api.send(client, walletID, m.dedup, m.body)
	}

	dead := testsupport.ReceiveAll(t, client, testsupport.InboundDLQ(t), 3)
	if len(dead) != 3 {
		t.Fatalf("dead-letter messages = %d, want 3", len(dead))
	}
	if got := api.balance(walletID); got != "100.00" {
		t.Errorf("balance = %s, want nothing moved by refused messages", got)
	}

	// Dead-lettered directly, so the inbound queue is clear well inside the
	// maxReceiveCount x VisibilityTimeout a redrive would have taken.
	inbound := testsupport.InboundQueue(t)
	out, err := client.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
		QueueUrl: &inbound, MaxNumberOfMessages: 10, WaitTimeSeconds: 1,
	})
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if len(out.Messages) != 0 {
		t.Errorf("messages still on the inbound queue = %d, want none", len(out.Messages))
	}
}

// the consumer dies after the commit and before the deletion. The
// redelivery must be a no-op, which is what the inbox is for.
func TestAMessageRedeliveredAfterACommitIsANoOp(t *testing.T) {
	api := startWagering(t)
	client := testsupport.SQSClient(t, testsupport.SQSEndpoint(t))
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	body := message("msg-1", "transaction-123", playerID, walletID, "BET", "25.00")
	api.send(client, walletID, "first", body)
	api.awaitBalance(walletID, "75.00")

	// The commit happened; this stands in for the delete that never ran.
	replays := metrics.Duplicates.Value()
	api.send(client, walletID, "redelivered", body)
	api.awaitReplayed(client, replays)

	if got := api.balance(walletID); got != "75.00" {
		t.Errorf("balance = %s, want the redelivery to have changed nothing", got)
	}
	if got := api.debits(walletID); got != 1 {
		t.Errorf("ledger debits = %d, want 1", got)
	}
	api.requireConsistent(walletID)
}

// awaitReplayed proves the repeat was received and deduplicated by the
// application: the replay counter moves by exactly one, then the message
// leaves the queue, which the consumer does only after handling it. Waiting on
// the balance instead would pass with a dead consumer.
func (w *wagering) awaitReplayed(client *awssqs.Client, before int64) {
	w.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for metrics.Duplicates.Value() == before {
		if time.Now().After(deadline) {
			w.t.Fatal("the repeat was never received and replayed after 30s")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if got := metrics.Duplicates.Value() - before; got != 1 {
		w.t.Errorf("replays = %d, want 1", got)
	}
	w.awaitInboundQueueDrained(client)
}

// awaitInboundQueueDrained waits for no message to be visible or in flight.
// A visible depth of zero alone also holds while the consumer still has it.
func (w *wagering) awaitInboundQueueDrained(client *awssqs.Client) {
	w.t.Helper()

	inbound := testsupport.InboundQueue(w.t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := client.GetQueueAttributes(context.Background(), &awssqs.GetQueueAttributesInput{
			QueueUrl: &inbound,
			AttributeNames: []types.QueueAttributeName{
				types.QueueAttributeNameApproximateNumberOfMessages,
				types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			},
		})
		if err != nil {
			w.t.Fatalf("read the queue depth: %v", err)
		}
		visible := out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)]
		inFlight := out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)]
		if visible == "0" && inFlight == "0" {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("queue after 30s: %s visible, %s in flight, want it drained", visible, inFlight)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// requireConsistent is the closing check 13 asks of every scenario: the stored
// balance equals the ledger's credits minus its debits.
func (w *wagering) requireConsistent(walletID string) {
	w.t.Helper()

	if report := w.reconcile(walletID); !report.Consistent {
		w.t.Errorf("reconciliation = %+v, want the stored balance to match the ledger", report)
	}
}

// on shutdown the consumer stops fetching and leaves nothing
// half-done — a message it never got to is still there for the next instance.
func TestShutdownStopsFetchingAndLeavesTheQueueIntact(t *testing.T) {
	api := startWagering(t)
	client := testsupport.SQSClient(t, testsupport.SQSEndpoint(t))
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	api.stop()

	api.send(client, walletID, "after-stop",
		message("msg-1", "transaction-123", playerID, walletID, "BET", "25.00"))

	// Nothing is consuming, so the message stays put.
	time.Sleep(2 * time.Second)
	inbound := testsupport.InboundQueue(t)
	out, err := client.ReceiveMessage(context.Background(), &awssqs.ReceiveMessageInput{
		QueueUrl:            &inbound,
		MaxNumberOfMessages: 10,
		WaitTimeSeconds:     1,
	})
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if len(out.Messages) != 1 {
		t.Errorf("messages left on the queue = %d, want the one nobody consumed", len(out.Messages))
	}
}

// inbox counts the rows the brief requires, which is how a test tells "handled once"
// from "handled twice with the same outcome".
func (w *wagering) inbox() int {
	w.t.Helper()

	conn := w.connect()
	defer conn.Close(context.Background())

	var rows int
	if err := conn.QueryRow(context.Background(),
		`SELECT count(*) FROM inbox_messages`).Scan(&rows); err != nil {
		w.t.Fatalf("count inbox rows: %v", err)
	}
	return rows
}

// holdWallet takes the same row lock a submission takes and keeps it, so every
// handling of a message for that wallet fails transiently until it is released.
func (w *wagering) holdWallet(walletID string) func() {
	w.t.Helper()

	ctx := context.Background()
	conn := w.connect()
	held, err := conn.Begin(ctx)
	if err != nil {
		w.t.Fatalf("begin: %v", err)
	}
	if _, err := held.Exec(ctx, "SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE", walletID); err != nil {
		w.t.Fatalf("hold the wallet: %v", err)
	}
	return func() {
		_ = held.Rollback(ctx)
		_ = conn.Close(ctx)
	}
}

// a confirmed business rejection is terminal, so its message leaves the
// queue — there is nothing a redelivery could decide differently.
func TestARejectedOperationRemovesItsMessage(t *testing.T) {
	api := startWagering(t)
	client := testsupport.SQSClient(t, testsupport.SQSEndpoint(t))
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "10.00")

	api.send(client, walletID, "overdrawn",
		message("msg-1", "transaction-123", playerID, walletID, "BET", "25.00"))

	api.awaitTransaction("provider-a", "transaction-123", "REJECTED", "INSUFFICIENT_FUNDS")
	api.awaitInboundQueueEmpty(client)
	if got := api.balance(walletID); got != "10.00" {
		t.Errorf("balance = %s, want nothing moved by a rejection", got)
	}
}

// a reversal that has to wait may finish its inbound message as soon as
// the pending state is committed; 13's worker takes it from there.
func TestAPendingReferenceRemovesItsMessageAndIsResolvedByTheWorker(t *testing.T) {
	api := startWagering(t)
	client := testsupport.SQSClient(t, testsupport.SQSEndpoint(t))
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	// The reversal overtakes the bet it undoes.
	api.send(client, walletID, "early-refund",
		refundMessage("msg-1", "refund-1", "bet-1", playerID, walletID, "25.00"))

	api.awaitTransaction("provider-a", "refund-1", "PENDING_REFERENCE", "")
	api.awaitInboundQueueEmpty(client)

	placed := api.bet(bet{
		externalID: "bet-1", key: "provider-a:bet-1",
		playerID: playerID, walletID: walletID, amount: "25.00",
	})
	if placed.Status != http.StatusOK {
		t.Fatalf("the reference failed: %+v", placed)
	}

	// Nothing is on the queue any more, so only the reference worker can move it.
	api.awaitTransaction("provider-a", "refund-1", "PROCESSED", "")
	if got := api.balance(walletID); got != "100.00" {
		t.Errorf("balance = %s, want the debit returned", got)
	}
}

// a failure that keeps failing runs out its attempts and the redrive
// policy moves it. The wallet lock is held throughout, so every handling is a
// genuine transient failure rather than a refusal the consumer would
// dead-letter itself.
func TestAMessageThatKeepsFailingIsRedrivenToTheDeadLetterQueue(t *testing.T) {
	t.Setenv("DB_LOCK_TIMEOUT", "300ms")
	api := startWagering(t)
	client := testsupport.SQSClient(t, testsupport.SQSEndpoint(t))
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	release := api.holdWallet(walletID)
	defer release()

	api.send(client, walletID, "contended",
		message("msg-1", "transaction-123", playerID, walletID, "BET", "25.00"))

	// SQSEnv provisions maxReceiveCount 2 with a 2s visibility timeout, so the
	// attempts are spent in a handful of seconds rather than the ninety
	// production would take.
	dead := testsupport.ReceiveAll(t, client, testsupport.InboundDLQ(t), 1)
	if len(dead) != 1 {
		t.Fatalf("dead-letter messages = %d, want the redriven one", len(dead))
	}
	if got := api.balance(walletID); got != "100.00" {
		t.Errorf("balance = %s, want nothing applied", got)
	}
}

// shutdown past the deadline abandons the handling in flight rather
// than half-applying it, and the message becomes visible again for another
// instance.
func TestShutdownReleasesAnInFlightMessageForRedelivery(t *testing.T) {
	// The handling is still waiting on the lock when the stop begins, and the
	// worker's own drain budget is what ends it — well inside the shutdown, so
	// the hooks after it still get their time and the stop stays clean.
	t.Setenv("DB_LOCK_TIMEOUT", "30s")
	t.Setenv("SHUTDOWN_TIMEOUT", "15s")
	t.Setenv("WORKER_DRAIN_TIMEOUT", "1s")
	api := startWagering(t)
	client := testsupport.SQSClient(t, testsupport.SQSEndpoint(t))
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	release := api.holdWallet(walletID)
	defer release()

	api.send(client, walletID, "in-flight",
		message("msg-1", "transaction-123", playerID, walletID, "BET", "25.00"))

	// Wait until the consumer has taken it: an invisible message is one being
	// handled.
	api.awaitInboundQueueEmpty(client)

	// RequireStop, not a tolerated failure: giving up on the handling must not
	// cost the rest of the shutdown its budget.
	api.stop()

	// The API is down, so what is left of the handling is read from the database.
	// The transaction was rolled back: no BET row, no movement, and the message
	// is back for whoever picks it up next.
	if got := api.transactionCount(); got != 1 {
		t.Errorf("wager transactions = %d, want only the opening", got)
	}
	if got := api.storedBalance(walletID); got != 10000 {
		t.Errorf("balance = %d minor units, want the untouched 10000", got)
	}
	api.awaitInboundQueueCount(client, 1)
}

// refundMessage is the envelope for a reversal, which adds the reference.
func refundMessage(messageID, externalID, reference, playerID, walletID, amount string) string {
	return fmt.Sprintf(`{"messageId":%q,"type":"WagerTransactionRequested",`+
		`"occurredAt":"2026-09-08T12:00:00.000Z","data":{`+
		`"providerId":"provider-a","externalTransactionId":%q,"idempotencyKey":"provider-a:%s",`+
		`"playerId":%q,"walletId":%q,"roundId":"round-987","gameId":"fortune-chimp",`+
		`"kind":"REFUND","money":{"amount":%q,"currency":"BRL"},`+
		`"referenceExternalTransactionId":%q}}`,
		messageID, externalID, externalID, playerID, walletID, amount, reference)
}

// awaitTransaction polls the provider read until the operation reaches a state,
// which is how a test observes work the queue path did asynchronously.
func (w *wagering) awaitTransaction(providerID, externalID, wantStatus, wantCode string) {
	w.t.Helper()

	deadline := time.Now().Add(30 * time.Second)
	for {
		status, code := w.providerTransaction(providerID, externalID)
		if status == wantStatus {
			if wantCode != "" && code != wantCode {
				w.t.Errorf("failureCode = %q, want %q", code, wantCode)
			}
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("transaction %s is %q after 30s, want %q", externalID, status, wantStatus)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (w *wagering) providerTransaction(providerID, externalID string) (status, code string) {
	w.t.Helper()

	resp, err := w.provider.Get(w.base + "/providers/" + providerID + "/wagering/transactions/" + externalID)
	if err != nil {
		w.t.Fatalf("GET provider transaction: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", ""
	}

	var read struct {
		Status      string `json:"status"`
		FailureCode string `json:"failureCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil {
		w.t.Fatalf("decode transaction: %v", err)
	}
	return read.Status, read.FailureCode
}

func (w *wagering) awaitInboundQueueEmpty(client *awssqs.Client) {
	w.awaitInboundQueueCount(client, 0)
}

// awaitInboundQueueCount waits for the queue to hold exactly want visible
// messages. Receiving would consume them, so the depth is read as an attribute.
func (w *wagering) awaitInboundQueueCount(client *awssqs.Client, want int) {
	w.t.Helper()

	inbound := testsupport.InboundQueue(w.t)
	deadline := time.Now().Add(30 * time.Second)
	for {
		out, err := client.GetQueueAttributes(context.Background(), &awssqs.GetQueueAttributesInput{
			QueueUrl:       &inbound,
			AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameApproximateNumberOfMessages},
		})
		if err != nil {
			w.t.Fatalf("read the queue depth: %v", err)
		}
		depth := out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)]
		if depth == strconv.Itoa(want) {
			return
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("visible messages = %s after 30s, want %d", depth, want)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (w *wagering) transactionCount() int {
	w.t.Helper()

	conn := w.connect()
	defer conn.Close(context.Background())

	var rows int
	if err := conn.QueryRow(context.Background(),
		`SELECT count(*) FROM wager_transactions`).Scan(&rows); err != nil {
		w.t.Fatalf("count wager transactions: %v", err)
	}
	return rows
}

// storedBalance reads the balance straight from the row, for the assertions a
// test makes after the API has stopped.
func (w *wagering) storedBalance(walletID string) int64 {
	w.t.Helper()

	conn := w.connect()
	defer conn.Close(context.Background())

	var minor int64
	if err := conn.QueryRow(context.Background(),
		`SELECT balance_minor FROM wallets WHERE id = $1`, walletID).Scan(&minor); err != nil {
		w.t.Fatalf("read the stored balance: %v", err)
	}
	return minor
}
