package wageringapp

import (
	"context"
	"testing"
	"time"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/events"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
)

// the reversal that arrived first waits, and is applied when its reference
// turns up — exactly once, under the full rules of 12.
func TestAWaitingReversalIsAppliedWhenItsReferenceArrives(t *testing.T) {
	service, repo, p := fixture(t, "100.00")

	waiting := submit(t, service, next(p, "refund-1", wagering.KindRefund, p.ExternalTransactionID))
	if waiting.Status() != wagering.StatusPendingReference {
		t.Fatalf("status = %s, want PENDING_REFERENCE", waiting.Status())
	}

	// Nothing to resolve to yet, so the sweep leaves it exactly as it was.
	if _, err := service.ResolveDue(context.Background(), 10); err != nil {
		t.Fatalf("ResolveDue() error = %v", err)
	}
	if waiting.Status() != wagering.StatusPendingReference {
		t.Fatalf("status = %s after a sweep with no reference, want PENDING_REFERENCE", waiting.Status())
	}

	bet := submit(t, service, p)
	resolved, err := service.ResolveDue(context.Background(), 10)
	if err != nil {
		t.Fatalf("ResolveDue() error = %v", err)
	}
	if resolved != 1 {
		t.Fatalf("resolved = %d, want 1", resolved)
	}
	if waiting.Status() != wagering.StatusProcessed {
		t.Fatalf("status = %s (%s), want PROCESSED", waiting.Status(), waiting.FailureCode())
	}
	if waiting.ReferenceTransactionID() != bet.ID() {
		t.Error("the resolved reversal did not record its reference")
	}
	if got := repo.wallet.Balance().String(); got != "100.00 BRL" {
		t.Errorf("balance = %s, want the debit returned", got)
	}

	// A second sweep finds nothing: the record is terminal.
	if resolved, err := service.ResolveDue(context.Background(), 10); err != nil || resolved != 0 {
		t.Errorf("a second sweep resolved %d (%v), want 0 and no re-application", resolved, err)
	}
	if got := repo.wallet.Balance().String(); got != "100.00 BRL" {
		t.Errorf("balance = %s after a second sweep, want 100.00 BRL", got)
	}
}

// the wait is bounded. On expiry the reversal ends REJECTED with the
// reference-not-found code and owes a rejection event.
func TestAWaitThatOutlivesTheTTLIsRejected(t *testing.T) {
	service, repo, p := fixture(t, "100.00")
	waiting := submit(t, service, next(p, "refund-1", wagering.KindRefund, "a-bet-never-sent"))
	if waiting.Status() != wagering.StatusPendingReference {
		t.Fatalf("status = %s, want PENDING_REFERENCE", waiting.Status())
	}
	written := len(repo.outbox)

	// The TTL runs from the operation's own createdAt, so moving the clock
	// forward is enough to expire it.
	service.now = func() time.Time { return waiting.CreatedAt().Add(25 * time.Hour) }

	if _, err := service.ResolveDue(context.Background(), 10); err != nil {
		t.Fatalf("ResolveDue() error = %v", err)
	}
	if waiting.Status() != wagering.StatusRejected {
		t.Fatalf("status = %s, want REJECTED", waiting.Status())
	}
	if got := waiting.FailureCode(); got != wagering.FailureReferenceNotFound {
		t.Errorf("failureCode = %s, want REFERENCE_NOT_FOUND", got)
	}
	if len(repo.outbox) != written+1 ||
		repo.outbox[written].EventType != events.TypeWagerTransactionRejected {
		t.Errorf("outbox = %v, want one WagerTransactionRejected appended", repo.outbox)
	}
	if len(repo.entries) != 0 {
		t.Errorf("ledger entries = %d, want none", len(repo.entries))
	}
}

// A wait short of the TTL is left alone — no state change, and no second
// WagerTransactionPendingReference on every sweep.
func TestAWaitInsideTheTTLIsLeftUntouched(t *testing.T) {
	service, repo, p := fixture(t, "100.00")
	waiting := submit(t, service, next(p, "refund-1", wagering.KindRefund, "a-bet-never-sent"))
	written := len(repo.outbox)

	for range 3 {
		if _, err := service.ResolveDue(context.Background(), 10); err != nil {
			t.Fatalf("ResolveDue() error = %v", err)
		}
	}
	if waiting.Status() != wagering.StatusPendingReference {
		t.Errorf("status = %s, want it still PENDING_REFERENCE", waiting.Status())
	}
	if len(repo.outbox) != written {
		t.Errorf("%d events were written by the sweeps, want none", len(repo.outbox)-written)
	}
}

// A.8.2: a reference that turned up terminal and unsuccessful ends the wait at
// once rather than burning the TTL on a certain rejection.
func TestAWaitEndsWhenItsReferenceFinishesUnsuccessfully(t *testing.T) {
	service, repo, p := fixture(t, "10.00")

	reversal := next(p, "refund-1", wagering.KindRefund, p.ExternalTransactionID)
	reversal.Money = brl(t, "25.00")
	waiting := submit(t, service, reversal)
	if waiting.Status() != wagering.StatusPendingReference {
		t.Fatalf("status = %s, want PENDING_REFERENCE", waiting.Status())
	}

	// The bet arrives and is rejected: the balance never covered it.
	if bet := submit(t, service, p); bet.Status() != wagering.StatusRejected {
		t.Fatalf("the reference ended %s, want REJECTED", bet.Status())
	}

	if _, err := service.ResolveDue(context.Background(), 10); err != nil {
		t.Fatalf("ResolveDue() error = %v", err)
	}
	if waiting.Status() != wagering.StatusRejected {
		t.Fatalf("status = %s, want REJECTED", waiting.Status())
	}
	if got := waiting.FailureCode(); got != wagering.FailureReferenceNotProcessed {
		t.Errorf("failureCode = %s, want REFERENCE_NOT_PROCESSED", got)
	}
	if len(repo.entries) != 0 {
		t.Errorf("ledger entries = %d, want none", len(repo.entries))
	}
}
