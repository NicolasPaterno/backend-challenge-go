//go:build integration && system

package system

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
)

// §13.1, over three processes and both transports. The amount is spelled three
// ways, because §9 and §10 make the hash the equality test and A.3.1 normalises
// "25", "25.0" and "25.00" to one amount — a hash over the received text would
// turn these into payload conflicts instead of replays.
func TestTheSameBetFiftyTimesAcrossInstancesDebitsOnce(t *testing.T) {
	c := startCluster(t)
	playerID := uuid.NewV7().String()
	walletID := c.openWallet(c.node(0), playerID, "100.00")

	spellings := []string{"25", "25.0", "25.00"}
	op := func(i int) operation {
		return operation{
			externalID: "transaction-123",
			key:        "provider-a:transaction-123",
			playerID:   playerID,
			walletID:   walletID,
			amount:     spellings[i%len(spellings)],
		}
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []outcome
	)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := c.submit(c.node(i), op(i))
			mu.Lock()
			results = append(results, result)
			mu.Unlock()
		}()
	}
	// The other ten arrive on the queue, each a message of its own, so the
	// deduplication being exercised is the application's and not FIFO's (§13).
	for i := 40; i < 50; i++ {
		c.enqueue(fmt.Sprintf("msg-%d", i), op(i))
	}
	wg.Wait()
	c.awaitQueueDrained()

	var processed, replays int
	for _, result := range results {
		switch {
		case result.Status != http.StatusOK:
			t.Errorf("status = %d, code = %s", result.Status, result.Code)
		case result.Replay:
			replays++
		default:
			processed++
		}
	}
	if processed != 1 {
		t.Errorf("fresh processings = %d, want exactly 1", processed)
	}
	if replays != 39 {
		t.Errorf("replays = %d, want 39", replays)
	}

	if balance, version := c.wallet(c.node(0), walletID); balance != "75.00" || version != 2 {
		t.Errorf("wallet = %s v%d, want 75.00 v2", balance, version)
	}
	// The opening credit and the single debit.
	if entries := c.ledgerEntries(walletID); entries != 2 {
		t.Errorf("ledger entries = %d, want 2", entries)
	}
}

// §8's mandatory race and §13.2, with each bet sent to a different process.
func TestTwoBetsOfEightyRaceForOneHundredAcrossInstances(t *testing.T) {
	c := startCluster(t)
	playerID := uuid.NewV7().String()
	walletID := c.openWallet(c.node(0), playerID, "100.00")

	bets := []operation{
		{externalID: "bet-a", playerID: playerID, walletID: walletID, amount: "80.00"},
		{externalID: "bet-b", playerID: playerID, walletID: walletID, amount: "80.00"},
	}

	race := func() []outcome {
		results := make([]outcome, len(bets))
		var wg sync.WaitGroup
		for i, b := range bets {
			wg.Add(1)
			go func() {
				defer wg.Done()
				results[i] = c.submit(c.node(i), b)
			}()
		}
		wg.Wait()
		return results
	}

	assert := func(stage string, results []outcome) {
		t.Helper()

		var accepted, refused int
		for _, result := range results {
			switch result.Status {
			case http.StatusOK:
				accepted++
			case http.StatusUnprocessableEntity:
				refused++
				if result.Code != "INSUFFICIENT_FUNDS" {
					t.Errorf("%s: rejection code = %s, want INSUFFICIENT_FUNDS", stage, result.Code)
				}
			default:
				t.Errorf("%s: status = %d, code = %s", stage, result.Status, result.Code)
			}
		}
		if accepted != 1 || refused != 1 {
			t.Errorf("%s: %d processed and %d rejected, want one of each", stage, accepted, refused)
		}
		if balance, _ := c.wallet(c.node(0), walletID); balance != "20.00" {
			t.Errorf("%s: balance = %s, want 20.00", stage, balance)
		}
		if entries := c.ledgerEntries(walletID); entries != 2 {
			t.Errorf("%s: ledger entries = %d, want the opening and one debit", stage, entries)
		}
	}

	assert("race", race())
	// §9: resubmitting both reproduces the stored outcome and moves nothing.
	assert("resubmission", race())
}

// §13.3 and §5.6: a wallet held by another writer must not hold up a different
// wallet. The lock is taken by the test itself, so the overlap is observed
// rather than assumed — the blocked bet is still waiting when the other
// instance answers for its own wallet.
func TestAHeldWalletDoesNotBlockAnother(t *testing.T) {
	c := startCluster(t)
	ctx := context.Background()

	playerA, playerB := uuid.NewV7().String(), uuid.NewV7().String()
	walletA := c.openWallet(c.node(0), playerA, "100.00")
	walletB := c.openWallet(c.node(0), playerB, "100.00")

	conn := c.connect()
	defer conn.Close(ctx)
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `SELECT id FROM wallets WHERE id = $1 FOR NO KEY UPDATE`, walletA); err != nil {
		t.Fatalf("hold wallet A: %v", err)
	}

	blocked := make(chan outcome, 1)
	go func() {
		blocked <- c.submit(c.node(0), operation{
			externalID: "held", playerID: playerA, walletID: walletA, amount: "10.00"})
	}()

	free := c.submit(c.node(1), operation{
		externalID: "free", playerID: playerB, walletID: walletB, amount: "10.00"})
	if free.Status != http.StatusOK {
		t.Fatalf("the unheld wallet answered %d (%s), want 200", free.Status, free.Code)
	}

	select {
	case result := <-blocked:
		t.Fatalf("the held wallet answered %d before the free one finished", result.Status)
	default:
	}

	// §8: waiting on the row is bounded, and a wallet held past the bound is
	// refused rather than queued forever.
	if result := <-blocked; result.Status != http.StatusServiceUnavailable {
		t.Errorf("the held wallet answered %d, want 503", result.Status)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("release wallet A: %v", err)
	}

	if balance, _ := c.wallet(c.node(0), walletB); balance != "90.00" {
		t.Errorf("wallet B = %s, want 90.00", balance)
	}
	if balance, _ := c.wallet(c.node(0), walletA); balance != "100.00" {
		t.Errorf("wallet A = %s, want 100.00 — the refused bet moved money", balance)
	}
}

// §13.5 and §13.4: one instance is killed outright while the queue is being
// worked, and every message is then redelivered to the survivors. A redelivery
// of work that had already committed must change nothing.
func TestAKilledConsumerLosesNoMessageAndDuplicatesNoDebit(t *testing.T) {
	c := startCluster(t)
	playerID := uuid.NewV7().String()
	walletID := c.openWallet(c.node(0), playerID, "100.00")

	const messages = 6
	send := func() {
		for i := range messages {
			id := strconv.Itoa(i)
			c.enqueue("msg-"+id, operation{
				externalID: "queued-" + id, playerID: playerID, walletID: walletID, amount: "5.00"})
		}
	}

	send()
	// The kill lands while the queue is being worked, one handling after the
	// first commit: whichever instance dies, no message may be lost and none
	// may be applied twice (§13.5).
	c.awaitCommittedTransactions(1)
	c.kill(c.nodes[0])

	// The same message ids again: proven repeated receipts, which is what §13
	// asks the deduplication be exercised with.
	c.awaitQueueDrained()
	send()
	c.awaitQueueDrained()

	if balance, _ := c.wallet(c.node(0), walletID); balance != "70.00" {
		t.Errorf("balance = %s, want 70.00 — one debit per message", balance)
	}
	if entries := c.ledgerEntries(walletID); entries != messages+1 {
		t.Errorf("ledger entries = %d, want %d", entries, messages+1)
	}
}

// §13.7 and §13.8: a reversal committed as PENDING_REFERENCE survives the death
// of the instance that accepted it, and another one takes it over.
func TestAPendingReferenceIsTakenOverByAnotherInstance(t *testing.T) {
	c := startCluster(t)
	playerID := uuid.NewV7().String()
	walletID := c.openWallet(c.node(0), playerID, "100.00")

	refund := c.submit(c.node(0), operation{
		externalID: "refund-1", playerID: playerID, walletID: walletID, amount: "30.00",
		kind: "REFUND", reference: "bet-late"})
	if refund.Status != http.StatusAccepted {
		t.Fatalf("REFUND before its reference answered %d (%s), want 202", refund.Status, refund.Code)
	}

	c.kill(c.nodes[0])

	bet := c.submit(c.node(0), operation{
		externalID: "bet-late", playerID: playerID, walletID: walletID, amount: "30.00"})
	if bet.Status != http.StatusOK {
		t.Fatalf("the reference answered %d (%s), want 200", bet.Status, bet.Code)
	}

	c.awaitStatus(c.node(0), refund.TransactionID, "PROCESSED")

	if balance, _ := c.wallet(c.node(0), walletID); balance != "100.00" {
		t.Errorf("balance = %s, want 100.00 — the bet debited and the refund returned it", balance)
	}

	// §13.8: idempotency is persistent, so the same bet under the same key
	// after the restart is a replay carrying its original balance.
	replay := c.submit(c.node(1), operation{
		externalID: "bet-late", playerID: playerID, walletID: walletID, amount: "30.00"})
	if !replay.Replay || replay.Balance != bet.Balance {
		t.Errorf("replay = %+v, want the original balance %s", replay, bet.Balance)
	}
}

// §13.6 and §11: three publishers drain one outbox, one of them dies mid-flight,
// and every event still reaches the queue exactly once under its own eventId.
func TestThreePublishersPublishEveryEventOnce(t *testing.T) {
	c := startCluster(t)
	playerID := uuid.NewV7().String()
	walletID := c.openWallet(c.node(0), playerID, "100.00")

	for i := range 8 {
		id := strconv.Itoa(i)
		if result := c.submit(c.node(i), operation{
			externalID: "pub-" + id, playerID: playerID, walletID: walletID, amount: "1.00"}); result.Status != http.StatusOK {
			t.Fatalf("bet %s answered %d (%s)", id, result.Status, result.Code)
		}
	}
	c.kill(c.nodes[0])

	expected := c.awaitOutboxDrained()
	messages := testsupport.ReceiveAll(t, c.sqs, c.events, expected)

	seen := make(map[string]int, expected)
	for _, m := range messages {
		var envelope struct {
			EventID string `json:"eventId"`
		}
		if err := json.Unmarshal([]byte(*m.Body), &envelope); err != nil {
			t.Fatalf("decode event: %v", err)
		}
		seen[envelope.EventID]++
	}
	if len(seen) != expected {
		t.Errorf("distinct events on the queue = %d, want %d", len(seen), expected)
	}
	for eventID, count := range seen {
		if count != 1 {
			t.Errorf("event %s was delivered %d times", eventID, count)
		}
	}
}

func (c *cluster) awaitQueueDrained() {
	c.t.Helper()

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		out, err := c.sqs.GetQueueAttributes(context.Background(), &awssqs.GetQueueAttributesInput{
			QueueUrl: &c.inbound,
			AttributeNames: []types.QueueAttributeName{
				types.QueueAttributeNameApproximateNumberOfMessages,
				types.QueueAttributeNameApproximateNumberOfMessagesNotVisible,
			},
		})
		if err != nil {
			c.t.Fatalf("read the inbound queue depth: %v", err)
		}
		available := out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessages)]
		inFlight := out.Attributes[string(types.QueueAttributeNameApproximateNumberOfMessagesNotVisible)]
		if available == "0" && inFlight == "0" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.t.Fatal("the inbound queue never drained")
}

func (c *cluster) awaitCommittedTransactions(want int) {
	c.t.Helper()

	ctx := context.Background()
	conn := c.connect()
	defer conn.Close(ctx)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var committed int
		if err := conn.QueryRow(ctx,
			`SELECT count(*) FROM wager_transactions WHERE origin = 'EXTERNAL'`).Scan(&committed); err != nil {
			c.t.Fatalf("count transactions: %v", err)
		}
		if committed >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.t.Fatalf("fewer than %d transactions committed", want)
}

// awaitOutboxDrained waits until no row is left unpublished and reports how many
// there are in total, which is what the queue must carry.
func (c *cluster) awaitOutboxDrained() int {
	c.t.Helper()

	ctx := context.Background()
	conn := c.connect()
	defer conn.Close(ctx)

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		var total, pending int
		if err := conn.QueryRow(ctx,
			`SELECT count(*), count(*) FILTER (WHERE published_at IS NULL) FROM outbox_events`).
			Scan(&total, &pending); err != nil {
			c.t.Fatalf("count outbox rows: %v", err)
		}
		if pending == 0 {
			return total
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.t.Fatal("the outbox never drained")
	return 0
}
