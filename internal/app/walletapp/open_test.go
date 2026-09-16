package walletapp

import (
	"context"
	"testing"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
)

type sequentialIDs struct{ n byte }

func (g *sequentialIDs) NewID() uuid.UUID {
	g.n++
	return uuid.UUID{15: g.n}
}

type recordingRepo struct {
	wallet  *wallet.Wallet
	opening *wagering.WagerTransaction
	entry   *wallet.LedgerEntry
	err     error
}

func (r *recordingRepo) Open(_ context.Context, w *wallet.Wallet, opening *wagering.WagerTransaction, entry *wallet.LedgerEntry) error {
	r.wallet, r.opening, r.entry = w, opening, entry
	return r.err
}

func (r *recordingRepo) ByID(context.Context, uuid.UUID) (*wallet.Wallet, error) {
	return r.wallet, r.err
}

func open(t *testing.T, amount string) (*wallet.Wallet, *recordingRepo) {
	t.Helper()

	balance, err := money.Parse(amount, money.BRL)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", amount, err)
	}
	repo := &recordingRepo{}
	opened, err := NewService(repo, &sequentialIDs{}).
		Open(context.Background(), OpenParams{PlayerID: uuid.NewV7(), InitialBalance: balance})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	return opened, repo
}

func TestOpenCreditsTheOpeningWithoutMovingTheVersionPastOne(t *testing.T) {
	opened, repo := open(t, "1000.00")

	if opened.Version() != 1 {
		t.Errorf("version = %d, want 1 (§9)", opened.Version())
	}
	if got := opened.Balance().String(); got != "1000.00 BRL" {
		t.Errorf("balance = %s, want 1000.00 BRL", got)
	}
	if repo.opening.Status() != wagering.StatusProcessed {
		t.Errorf("opening status = %s, want PROCESSED", repo.opening.Status())
	}
	if repo.opening.Origin() != wagering.OriginInternal || repo.opening.Kind() != wagering.KindOpening {
		t.Errorf("opening = %s/%s, want INTERNAL/OPENING", repo.opening.Origin(), repo.opening.Kind())
	}
	if repo.entry.TransactionID() != repo.opening.ID() {
		t.Errorf("entry transactionId = %s, want the opening id %s", repo.entry.TransactionID(), repo.opening.ID())
	}
	if repo.entry.Direction() != wallet.DirectionCredit || !repo.entry.BalanceBefore().IsZero() {
		t.Errorf("entry = %s from %s, want a CREDIT from zero", repo.entry.Direction(), repo.entry.BalanceBefore())
	}
}

func TestOpenWithAZeroBalanceWritesNoOpeningAndNoEntry(t *testing.T) {
	opened, repo := open(t, "0.00")

	if repo.opening != nil || repo.entry != nil {
		t.Errorf("opening = %v, entry = %v, want neither (§9)", repo.opening, repo.entry)
	}
	if opened.Version() != 1 || !opened.Balance().IsZero() {
		t.Errorf("wallet = %s at version %d, want 0.00 at version 1", opened.Balance(), opened.Version())
	}
}
