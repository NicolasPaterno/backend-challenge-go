package walletapp

import (
	"context"
	"slices"
	"testing"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/events"
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
	outbox  []events.Envelope
	limit   int
	err     error

	stored, calculated money.Money
	entries            int
}

func (r *recordingRepo) Open(_ context.Context, w *wallet.Wallet, opening *wagering.WagerTransaction, entry *wallet.LedgerEntry, outbox []events.Envelope) error {
	r.wallet, r.opening, r.entry, r.outbox = w, opening, entry, outbox
	return r.err
}

func (r *recordingRepo) ByID(context.Context, uuid.UUID) (*wallet.Wallet, error) {
	return r.wallet, r.err
}

func (r *recordingRepo) Ledger(_ context.Context, _ uuid.UUID, _ *LedgerCursor, limit int) ([]*wallet.LedgerEntry, error) {
	r.limit = limit
	if r.entry == nil {
		return nil, r.err
	}
	return []*wallet.LedgerEntry{r.entry}, r.err
}

func (r *recordingRepo) Reconcile(context.Context, uuid.UUID) (money.Money, money.Money, int, error) {
	return r.stored, r.calculated, r.entries, r.err
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
		t.Errorf("version = %d, want 1", opened.Version())
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
		t.Errorf("opening = %v, entry = %v, want neither", repo.opening, repo.entry)
	}
	if opened.Version() != 1 || !opened.Balance().IsZero() {
		t.Errorf("wallet = %s at version %d, want 0.00 at version 1", opened.Balance(), opened.Version())
	}
}

// the opening's two events share the wallet's commit, and a zero opening
// writes neither.
func TestOpenWritesItsEventsToTheOutbox(t *testing.T) {
	_, repo := open(t, "1000.00")

	var types []string
	for _, e := range repo.outbox {
		types = append(types, e.EventType)
		if e.AggregateID != repo.wallet.ID() {
			t.Errorf("%s aggregateId = %s, want the wallet", e.EventType, e.AggregateID)
		}
		if e.CorrelationID != repo.opening.ID() {
			t.Errorf("%s correlationId = %s, want the OPENING's id (A.1)", e.EventType, e.CorrelationID)
		}
	}
	want := []string{events.TypeWagerTransactionProcessed, events.TypeWalletBalanceChanged}
	if !slices.Equal(types, want) {
		t.Errorf("outbox = %v, want %v", types, want)
	}

	balance, ok := repo.outbox[1].Data.(events.WalletBalanceChanged)
	if !ok {
		t.Fatalf("data is %T, want events.WalletBalanceChanged", repo.outbox[1].Data)
	}
	if balance.WalletVersion != 1 {
		t.Errorf("walletVersion = %d, want the 1 an opening is pinned at", balance.WalletVersion)
	}
	if got := balance.BalanceAfter.String(); got != "1000.00 BRL" {
		t.Errorf("balanceAfter = %s, want 1000.00 BRL", got)
	}

	_, zero := open(t, "0.00")
	if len(zero.outbox) != 0 {
		t.Errorf("a zero opening wrote %d events, want none", len(zero.outbox))
	}
}
