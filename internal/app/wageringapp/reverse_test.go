package wageringapp

import (
	"context"
	"testing"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/events"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
)

// submit runs one operation and fails the test if the use case itself errored;
// a business rejection is an outcome, not an error, so it comes back in Result.
func submit(t *testing.T, service *Service, p SubmitParams) *wagering.WagerTransaction {
	t.Helper()

	result, err := service.Submit(context.Background(), p)
	if err != nil {
		t.Fatalf("Submit(%s %s) error = %v", p.Kind, p.ExternalTransactionID, err)
	}
	return result.Transaction
}

// next derives a follow-up operation from p: its own external id and key, and
// the reference it cites.
func next(p SubmitParams, id string, kind wagering.Kind, reference string) SubmitParams {
	p.ExternalTransactionID = id
	p.IdempotencyKey = "provider-a:" + id
	p.Kind = kind
	p.ReferenceExternalTransactionID = reference
	return p
}

func TestRefundReturnsTheDebitOfItsBet(t *testing.T) {
	service, repo, p := fixture(t, "100.00")
	bet := submit(t, service, p)

	refund := submit(t, service, next(p, "refund-1", wagering.KindRefund, p.ExternalTransactionID))
	if refund.Status() != wagering.StatusProcessed {
		t.Fatalf("status = %s (%s), want PROCESSED", refund.Status(), refund.FailureCode())
	}
	if refund.ReferenceTransactionID() != bet.ID() {
		t.Error("the refund did not record the internal id of its reference")
	}
	if got := repo.wallet.Balance().String(); got != "100.00 BRL" {
		t.Errorf("balance = %s, want the 100.00 BRL the bet started from", got)
	}
	// §5.5: the debit stays and the return is a second entry, never an edit.
	if len(repo.entries) != 2 {
		t.Errorf("ledger entries = %d, want 2", len(repo.entries))
	}
}

// §7: a ROLLBACK applies the movement opposite to the one it undoes.
func TestRollbackAppliesTheOppositeMovement(t *testing.T) {
	tests := map[string]struct {
		kind wagering.Kind
		want string
	}{
		"of a bet":    {wagering.KindBet, "100.00 BRL"},
		"of a win":    {wagering.KindWin, "100.00 BRL"},
		"of a refund": {wagering.KindRefund, "75.00 BRL"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			service, repo, p := fixture(t, "100.00")

			target := p
			if test.kind == wagering.KindRefund {
				submit(t, service, p)
				target = next(p, "refund-1", wagering.KindRefund, p.ExternalTransactionID)
			} else {
				target.Kind = test.kind
			}
			reversed := submit(t, service, target)
			if reversed.Status() != wagering.StatusProcessed {
				t.Fatalf("the reference ended %s (%s)", reversed.Status(), reversed.FailureCode())
			}

			rollback := submit(t, service, next(p, "rollback-1", wagering.KindRollback, target.ExternalTransactionID))
			if rollback.Status() != wagering.StatusProcessed {
				t.Fatalf("status = %s (%s), want PROCESSED", rollback.Status(), rollback.FailureCode())
			}
			if got := repo.wallet.Balance().String(); got != test.want {
				t.Errorf("balance = %s, want %s", got, test.want)
			}
		})
	}
}

// A.8.1: the bet's debit is returned once, by either kind. The second reversal
// is refused whatever kind it carries, which a per-kind rule would let through.
func TestOneSuccessfulReversalSpendsTheReference(t *testing.T) {
	for _, second := range []wagering.Kind{wagering.KindRefund, wagering.KindRollback} {
		t.Run(second.String(), func(t *testing.T) {
			service, repo, p := fixture(t, "100.00")
			submit(t, service, p)
			submit(t, service, next(p, "refund-1", wagering.KindRefund, p.ExternalTransactionID))

			again := submit(t, service, next(p, "second-1", second, p.ExternalTransactionID))
			if again.Status() != wagering.StatusRejected {
				t.Fatalf("status = %s, want REJECTED", again.Status())
			}
			if got := again.FailureCode(); got != wagering.FailureReferenceAlreadyReversed {
				t.Errorf("failureCode = %s, want REFERENCE_ALREADY_REVERSED", got)
			}
			if got := repo.wallet.Balance().String(); got != "100.00 BRL" {
				t.Errorf("balance = %s, want the debit returned exactly once", got)
			}
		})
	}
}

// A ROLLBACK of the REFUND is how §7 undoes a refund, and it is not blocked by
// the rule above: the REFUND is a reference of its own.
func TestARefundIsUndoneByRollingItBack(t *testing.T) {
	service, repo, p := fixture(t, "100.00")
	submit(t, service, p)
	submit(t, service, next(p, "refund-1", wagering.KindRefund, p.ExternalTransactionID))

	rollback := submit(t, service, next(p, "rollback-1", wagering.KindRollback, "refund-1"))
	if rollback.Status() != wagering.StatusProcessed {
		t.Fatalf("status = %s (%s), want PROCESSED", rollback.Status(), rollback.FailureCode())
	}
	if got := repo.wallet.Balance().String(); got != "75.00 BRL" {
		t.Errorf("balance = %s, want the bet standing again at 75.00 BRL", got)
	}
}

// §7's agreement rules and the reversal table, each broken one field at a time.
func TestReversalRefusesAReferenceItDoesNotMatch(t *testing.T) {
	tests := map[string]struct {
		referenceKind wagering.Kind
		kind          wagering.Kind
		break_        func(*testing.T, *SubmitParams)
		want          wagering.FailureCode
	}{
		"another round": {wagering.KindBet, wagering.KindRefund,
			func(_ *testing.T, p *SubmitParams) { p.RoundID = "round-000" }, wagering.FailureReferenceMismatch},
		"another wallet": {wagering.KindBet, wagering.KindRefund,
			func(_ *testing.T, p *SubmitParams) { p.WalletID = p.PlayerID }, wagering.FailureWalletNotFound},
		"a different amount": {wagering.KindBet, wagering.KindRefund,
			func(t *testing.T, p *SubmitParams) { p.Money = brl(t, "10.00") }, wagering.FailureReferenceAmount},
		// §7 makes a WIN a rollback target, never a refund target.
		"a refund of a win": {wagering.KindWin, wagering.KindRefund,
			func(*testing.T, *SubmitParams) {}, wagering.FailureReferenceMismatch},
		// A LOSS moved nothing, so there is nothing to undo.
		"a rollback of a loss": {wagering.KindLoss, wagering.KindRollback,
			func(*testing.T, *SubmitParams) {}, wagering.FailureReferenceMismatch},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			service, repo, p := fixture(t, "100.00")
			reference := p
			reference.Kind = test.referenceKind
			if test.referenceKind == wagering.KindLoss {
				reference.Money = brl(t, "0.00")
			}
			submit(t, service, reference)
			before := repo.wallet.Balance().String()

			reversal := next(p, "reversal-1", test.kind, p.ExternalTransactionID)
			test.break_(t, &reversal)

			result := submit(t, service, reversal)
			if result.Status() != wagering.StatusRejected {
				t.Fatalf("status = %s, want REJECTED", result.Status())
			}
			if got := result.FailureCode(); got != test.want {
				t.Errorf("failureCode = %s, want %s", got, test.want)
			}
			if got := repo.wallet.Balance().String(); got != before {
				t.Errorf("balance = %s, want the untouched %s", got, before)
			}
		})
	}
}

// §7: a reversal that cannot be covered is rejected with a code of its own, so
// an incident is not read as a routine bet without funds.
func TestAReversalThatOverdrawsIsRejectedWithItsOwnCode(t *testing.T) {
	service, repo, p := fixture(t, "100.00")
	win := next(p, "win-1", wagering.KindWin, "")
	win.Money = brl(t, "100.00")
	submit(t, service, win)

	spend := next(p, "bet-2", wagering.KindBet, "")
	spend.Money = brl(t, "150.00")
	submit(t, service, spend)

	undo := next(p, "rollback-1", wagering.KindRollback, "win-1")
	undo.Money = brl(t, "100.00")

	rollback := submit(t, service, undo)
	if got := rollback.FailureCode(); got != wagering.FailureReversalExceedsBalance {
		t.Fatalf("failureCode = %s, want REVERSAL_EXCEEDS_BALANCE", got)
	}
	if rollback.FailureCode() == wagering.FailureInsufficientFunds {
		t.Error("the reversal reused the bet's insufficient-funds code")
	}
	if got := repo.wallet.Balance().String(); got != "50.00 BRL" {
		t.Errorf("balance = %s, want the untouched 50.00 BRL", got)
	}
}

// A.8.2: a reference that is terminal and not PROCESSED can never become
// processed, so waiting for it only delays a certain rejection until the TTL.
// The code is its own, distinct from the reference-not-found of a wait.
func TestAReversalOfANonProcessedReferenceIsRejectedAtOnce(t *testing.T) {
	service, repo, p := fixture(t, "10.00")
	rejected := submit(t, service, p)
	if rejected.Status() != wagering.StatusRejected {
		t.Fatalf("the reference ended %s, want REJECTED for the test to mean anything", rejected.Status())
	}

	reversal := submit(t, service, next(p, "refund-1", wagering.KindRefund, p.ExternalTransactionID))
	if reversal.Status() != wagering.StatusRejected {
		t.Fatalf("status = %s, want REJECTED rather than a wait", reversal.Status())
	}
	if got := reversal.FailureCode(); got != wagering.FailureReferenceNotProcessed {
		t.Errorf("failureCode = %s, want REFERENCE_NOT_PROCESSED", got)
	}
	if reversal.ReferenceTransactionID() != rejected.ID() {
		t.Error("the rejected reversal did not record what it pointed at")
	}
	if len(repo.entries) != 0 {
		t.Errorf("ledger entries = %d, want none", len(repo.entries))
	}
}

// A.8.2: absent and not-yet-finished are the same case — the reference is not
// available, and §7 waits for it. 13 adds the worker that retries and expires.
func TestAReversalWaitsForAReferenceThatHasNotArrived(t *testing.T) {
	service, repo, p := fixture(t, "100.00")

	reversal := submit(t, service, next(p, "refund-1", wagering.KindRefund, "a-bet-still-in-flight"))
	if reversal.Status() != wagering.StatusPendingReference {
		t.Fatalf("status = %s, want PENDING_REFERENCE", reversal.Status())
	}
	if reversal.FailureCode() != "" {
		t.Errorf("failureCode = %s, want none on a wait", reversal.FailureCode())
	}
	if len(repo.entries) != 0 {
		t.Errorf("ledger entries = %d, want none", len(repo.entries))
	}
	if len(repo.outbox) != 1 || repo.outbox[0].EventType != events.TypeWagerTransactionPendingReference {
		t.Errorf("outbox = %v, want one WagerTransactionPendingReference", repo.outbox)
	}
}
