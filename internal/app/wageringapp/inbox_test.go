package wageringapp

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
)

func onMessage(p SubmitParams, messageID string) SubmitParams {
	p.Inbox = Inbox{MessageID: messageID, ReceivedAt: time.Now().UTC()}
	return p
}

// §10: a redelivery of the same message moves money once. The inbox is what
// stops it before the work is repeated.
func TestARedeliveredMessageIsHandledOnce(t *testing.T) {
	service, repo, p := fixture(t, "100.00")
	delivered := onMessage(p, "msg-123")

	first, err := service.Submit(context.Background(), delivered)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if first.Replay {
		t.Error("the first delivery reported a replay")
	}

	again, err := service.Submit(context.Background(), delivered)
	if err != nil {
		t.Fatalf("redelivery Submit() error = %v", err)
	}
	if !again.Replay {
		t.Error("the redelivery did not report a replay")
	}
	if got := repo.wallet.Balance().String(); got != "75.00 BRL" {
		t.Errorf("balance = %s, want one debit", got)
	}
	if len(repo.entries) != 1 {
		t.Errorf("ledger entries = %d, want 1", len(repo.entries))
	}
}

// §10: the hash is verified on a redelivery, so one message id carrying
// different content is refused rather than treated as already handled.
func TestAMessageIDReusedWithOtherContentIsRefused(t *testing.T) {
	service, repo, p := fixture(t, "100.00")
	if _, err := service.Submit(context.Background(), onMessage(p, "msg-123")); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	changed := onMessage(p, "msg-123")
	changed.ExternalTransactionID = "transaction-999"
	changed.IdempotencyKey = "provider-a:transaction-999"

	if _, err := service.Submit(context.Background(), changed); !errors.Is(err, ErrMessageConflict) {
		t.Errorf("error = %v, want ErrMessageConflict", err)
	}
	if got := repo.wallet.Balance().String(); got != "75.00 BRL" {
		t.Errorf("balance = %s, want the second message to have moved nothing", got)
	}
}

// §10 requires HTTP and SQS to share the idempotency guarantees, which rests on
// the two paths hashing an operation identically: the inbox identity is
// transport metadata and must not reach the digest (§9).
func TestTheInboxIdentityIsOutsideTheHash(t *testing.T) {
	_, _, p := fixture(t, "100.00")

	if PayloadHash(p) != PayloadHash(onMessage(p, "msg-123")) {
		t.Fatal("the messageId changed the hash; §9 excludes transport metadata")
	}

	service, repo, _ := fixture(t, "100.00")
	_, _, overHTTP := fixture(t, "100.00")
	overHTTP.PlayerID, overHTTP.WalletID = repo.wallet.PlayerID(), repo.wallet.ID()

	if _, err := service.Submit(context.Background(), overHTTP); err != nil {
		t.Fatalf("HTTP Submit() error = %v", err)
	}
	// The same operation arriving on the queue: one financial effect (§10).
	replay, err := service.Submit(context.Background(), onMessage(overHTTP, "msg-123"))
	if err != nil {
		t.Fatalf("SQS Submit() error = %v", err)
	}
	if !replay.Replay {
		t.Error("the queue delivery did not replay the HTTP result")
	}
	if len(repo.entries) != 1 {
		t.Errorf("ledger entries = %d, want 1 for one operation over two transports", len(repo.entries))
	}
}

// §6.3: OPENING is refused whatever transport carries it.
func TestOpeningIsRefusedOnTheQueueToo(t *testing.T) {
	service, _, p := fixture(t, "100.00")
	p.Kind = wagering.KindOpening

	if _, err := service.Submit(context.Background(), onMessage(p, "msg-123")); !errors.Is(err, wagering.ErrOpeningIsInternal) {
		t.Errorf("error = %v, want ErrOpeningIsInternal", err)
	}
}
