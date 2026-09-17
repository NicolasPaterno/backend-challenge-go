package wageringapp

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

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

// fakeRepo stands in for the two unique indexes and the locked wallet row. The
// real guarantees are exercised against PostgreSQL in cmd/api.
type fakeRepo struct {
	wallet     *wallet.Wallet
	entries    []*wallet.LedgerEntry
	outbox     []events.Envelope
	byKey      map[string]*wagering.WagerTransaction
	byExternal map[string]*wagering.WagerTransaction
}

func newFakeRepo(w *wallet.Wallet) *fakeRepo {
	return &fakeRepo{
		wallet:     w,
		byKey:      map[string]*wagering.WagerTransaction{},
		byExternal: map[string]*wagering.WagerTransaction{},
	}
}

func (r *fakeRepo) Process(_ context.Context, t *wagering.WagerTransaction, decide Decide) error {
	if r.byKey[t.ProviderID()+"|"+t.IdempotencyKey()] != nil ||
		r.byExternal[t.ProviderID()+"|"+t.ExternalTransactionID()] != nil {
		return ErrDuplicate
	}
	found := r.wallet
	if found != nil && found.ID() != t.WalletID() {
		found = nil
	}
	var ref *Reference
	if t.Kind().IsReversal() {
		if referenced := r.byExternal[t.ProviderID()+"|"+t.ReferenceExternalTransactionID()]; referenced != nil {
			ref = &Reference{Transaction: referenced, Reversed: r.reversed(referenced.ID())}
		}
	}

	entry, outbox, err := decide(found, ref)
	if err != nil {
		return err
	}
	if entry != nil {
		r.entries = append(r.entries, entry)
	}
	r.outbox = append(r.outbox, outbox...)
	r.byKey[t.ProviderID()+"|"+t.IdempotencyKey()] = t
	r.byExternal[t.ProviderID()+"|"+t.ExternalTransactionID()] = t
	return nil
}

// A.8.1's rule as the partial unique index of 0009 enforces it: any successful
// reversal spends the reference, whatever its kind.
func (r *fakeRepo) reversed(referenceID uuid.UUID) bool {
	for _, t := range r.byKey {
		if t.Kind().IsReversal() && t.Status() == wagering.StatusProcessed &&
			t.ReferenceTransactionID() == referenceID {
			return true
		}
	}
	return false
}

func (r *fakeRepo) ByID(_ context.Context, id uuid.UUID) (*wagering.WagerTransaction, error) {
	for _, t := range r.byKey {
		if t.ID() == id {
			return t, nil
		}
	}
	return nil, ErrNotFound
}

func (r *fakeRepo) ByIdempotencyKey(_ context.Context, providerID, key string) (*wagering.WagerTransaction, error) {
	if t := r.byKey[providerID+"|"+key]; t != nil {
		return t, nil
	}
	return nil, ErrNotFound
}

func (r *fakeRepo) ByExternalID(_ context.Context, providerID, externalID string) (*wagering.WagerTransaction, error) {
	if t := r.byExternal[providerID+"|"+externalID]; t != nil {
		return t, nil
	}
	return nil, ErrNotFound
}

func brl(t *testing.T, amount string) money.Money {
	t.Helper()

	parsed, err := money.Parse(amount, money.BRL)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", amount, err)
	}
	return parsed
}

func fixture(t *testing.T, balance string) (*Service, *fakeRepo, SubmitParams) {
	t.Helper()

	playerID, walletID := uuid.NewV7(), uuid.NewV7()
	w, err := wallet.Rehydrate(walletID, playerID, brl(t, balance), 1, time.Now().UTC(), time.Now().UTC())
	if err != nil {
		t.Fatalf("Rehydrate() error = %v", err)
	}

	repo := newFakeRepo(w)
	return NewService(repo, &sequentialIDs{}), repo, SubmitParams{
		ProviderID:            "provider-a",
		ExternalTransactionID: "transaction-123",
		IdempotencyKey:        "provider-a:transaction-123",
		PlayerID:              playerID,
		WalletID:              walletID,
		RoundID:               "round-987",
		GameID:                "fortune-chimp",
		Kind:                  wagering.KindBet,
		Money:                 brl(t, "25.00"),
	}
}

func TestPayloadHashIsTakenOverNormalisedMoney(t *testing.T) {
	_, _, p := fixture(t, "100.00")

	spelled := p
	spelled.Money = brl(t, "25")
	if PayloadHash(p) != PayloadHash(spelled) {
		t.Error(`"25" and "25.00" hashed differently; the hash must be over minor units (A.3.1)`)
	}

	rekeyed := p
	rekeyed.IdempotencyKey = "a-completely-different-key"
	if PayloadHash(p) != PayloadHash(rekeyed) {
		t.Error("the idempotency key changed the hash; §9 excludes it")
	}

	other := p
	other.Money = brl(t, "25.01")
	if PayloadHash(p) == PayloadHash(other) {
		t.Error("25.00 and 25.01 hashed alike")
	}
}

func TestSubmitDebitsAndReplaysTheStoredResult(t *testing.T) {
	service, _, p := fixture(t, "100.00")

	first, err := service.Submit(context.Background(), p)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if first.Replay {
		t.Error("the first submission reported a replay")
	}
	if first.Transaction.Status() != wagering.StatusProcessed {
		t.Fatalf("status = %s, want PROCESSED", first.Transaction.Status())
	}
	if got := first.Transaction.ResultBalance().String(); got != "75.00 BRL" {
		t.Errorf("resultBalance = %s, want 75.00 BRL", got)
	}

	// The same operation spelled differently under the same key: a replay, not
	// a conflict (A.3.1).
	resent := p
	resent.Money = brl(t, "25")
	second, err := service.Submit(context.Background(), resent)
	if err != nil {
		t.Fatalf("replay Submit() error = %v", err)
	}
	if !second.Replay {
		t.Error("the resubmission did not report a replay")
	}
	if got := second.Transaction.ResultBalance().String(); got != "75.00 BRL" {
		t.Errorf("replayed balance = %s, want the 75.00 BRL observed at processing", got)
	}
}

func TestSubmitRefusesAReusedKeyAndASecondKey(t *testing.T) {
	service, _, p := fixture(t, "100.00")
	if _, err := service.Submit(context.Background(), p); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	changed := p
	changed.Money = brl(t, "30.00")
	if _, err := service.Submit(context.Background(), changed); !errors.Is(err, ErrPayloadConflict) {
		t.Errorf("error = %v, want ErrPayloadConflict", err)
	}

	rekeyed := p
	rekeyed.IdempotencyKey = "provider-a:another-key"
	if _, err := service.Submit(context.Background(), rekeyed); !errors.Is(err, ErrExternalIDConflict) {
		t.Errorf("error = %v, want ErrExternalIDConflict", err)
	}
}

func TestSubmitRejectsABetItCannotCover(t *testing.T) {
	service, repo, p := fixture(t, "10.00")

	result, err := service.Submit(context.Background(), p)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if result.Transaction.Status() != wagering.StatusRejected {
		t.Fatalf("status = %s, want REJECTED", result.Transaction.Status())
	}
	if got := result.Transaction.FailureCode(); got != wagering.FailureInsufficientFunds {
		t.Errorf("failureCode = %s, want INSUFFICIENT_FUNDS", got)
	}
	// 03 §3: a refused debit leaves the aggregate untouched.
	if got := repo.wallet.Balance().String(); got != "10.00 BRL" {
		t.Errorf("balance = %s, want 10.00 BRL", got)
	}
	if repo.wallet.Version() != 1 {
		t.Errorf("version = %d, want 1", repo.wallet.Version())
	}
}

// §11 owes a rejection event to both, so both must be recorded, not returned as
// an error. One code for the two denies an enumeration oracle (§2).
func TestSubmitRecordsWalletNotFound(t *testing.T) {
	tests := map[string]func(*SubmitParams){
		"another player's wallet": func(p *SubmitParams) { p.PlayerID = uuid.NewV7() },
		"no such wallet":          func(p *SubmitParams) { p.WalletID = uuid.NewV7() },
	}

	for name, break_ := range tests {
		t.Run(name, func(t *testing.T) {
			service, _, p := fixture(t, "100.00")
			break_(&p)

			result, err := service.Submit(context.Background(), p)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if result.Transaction.Status() != wagering.StatusRejected {
				t.Errorf("status = %s, want REJECTED", result.Transaction.Status())
			}
			if got := result.Transaction.FailureCode(); got != wagering.FailureWalletNotFound {
				t.Errorf("failureCode = %s, want WALLET_NOT_FOUND", got)
			}
			if result.Transaction.ResultBalance().IsValid() {
				t.Error("a rejection with no wallet reported a balance")
			}
		})
	}
}

func TestSubmitCreditsAWin(t *testing.T) {
	service, repo, p := fixture(t, "100.00")
	p.Kind = wagering.KindWin
	p.ReferenceExternalTransactionID = "bet-of-the-round"

	result, err := service.Submit(context.Background(), p)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if result.Transaction.Status() != wagering.StatusProcessed {
		t.Fatalf("status = %s, want PROCESSED", result.Transaction.Status())
	}
	if got := result.Transaction.ResultBalance().String(); got != "125.00 BRL" {
		t.Errorf("resultBalance = %s, want 125.00 BRL", got)
	}
	if got := repo.wallet.Balance().String(); got != "125.00 BRL" {
		t.Errorf("balance = %s, want 125.00 BRL", got)
	}
	if repo.wallet.Version() != 2 {
		t.Errorf("version = %d, want 2", repo.wallet.Version())
	}
	if len(repo.entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(repo.entries))
	}
	if got := repo.entries[0].Direction(); got != wallet.DirectionCredit {
		t.Errorf("direction = %s, want CREDIT", got)
	}
	if result.Transaction.ReferenceExternalTransactionID() != "bet-of-the-round" {
		t.Error("the WIN's reference was not recorded")
	}
	if result.Transaction.ReferenceTransactionID() != uuid.Nil() {
		t.Error("the WIN's reference was resolved; 09 only records it (A.4)")
	}
}

func TestSubmitSettlesALossWithoutMovingMoney(t *testing.T) {
	service, repo, p := fixture(t, "100.00")
	p.Kind = wagering.KindLoss
	p.Money = brl(t, "0")

	result, err := service.Submit(context.Background(), p)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if result.Transaction.Status() != wagering.StatusProcessed {
		t.Fatalf("status = %s, want PROCESSED", result.Transaction.Status())
	}
	if got := result.Transaction.ResultBalance().String(); got != "100.00 BRL" {
		t.Errorf("resultBalance = %s, want the unchanged 100.00 BRL", got)
	}
	if got := repo.wallet.Balance().String(); got != "100.00 BRL" {
		t.Errorf("balance = %s, want 100.00 BRL", got)
	}
	if repo.wallet.Version() != 1 {
		t.Errorf("version = %d, want 1", repo.wallet.Version())
	}
	if len(repo.entries) != 0 {
		t.Errorf("ledger entries = %d, want none", len(repo.entries))
	}
}

func TestSubmitRejectsALossInTheWrongCurrency(t *testing.T) {
	service, _, p := fixture(t, "100.00")
	p.Kind = wagering.KindLoss

	zero, err := money.Parse("0.00", money.EUR)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	p.Money = zero

	result, err := service.Submit(context.Background(), p)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got := result.Transaction.FailureCode(); got != wagering.FailureCurrencyMismatch {
		t.Errorf("failureCode = %s, want WALLET_CURRENCY_MISMATCH", got)
	}
}

func TestSubmitRefusesOpening(t *testing.T) {
	service, _, p := fixture(t, "100.00")
	p.Kind = wagering.KindOpening

	if _, err := service.Submit(context.Background(), p); !errors.Is(err, wagering.ErrOpeningIsInternal) {
		t.Errorf("Submit(OPENING) error = %v, want ErrOpeningIsInternal", err)
	}
}

// §11: the outcome decides the events, and they are handed to the repository
// for the commit that carries the movement (§5.4).
func TestSubmitWritesTheEventsItsOutcomeOwes(t *testing.T) {
	tests := map[string]struct {
		balance string
		kind    wagering.Kind
		amount  string
		want    []string
	}{
		"bet": {"100.00", wagering.KindBet, "25.00",
			[]string{events.TypeWagerTransactionProcessed, events.TypeWalletBalanceChanged}},
		"win": {"100.00", wagering.KindWin, "25.00",
			[]string{events.TypeWagerTransactionProcessed, events.TypeWalletBalanceChanged}},
		// §7: a LOSS produces WagerTransactionProcessed and no balance change.
		"loss": {"100.00", wagering.KindLoss, "0.00",
			[]string{events.TypeWagerTransactionProcessed}},
		"rejection": {"10.00", wagering.KindBet, "25.00",
			[]string{events.TypeWagerTransactionRejected}},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			service, repo, p := fixture(t, test.balance)
			p.Kind, p.Money = test.kind, brl(t, test.amount)

			result, err := service.Submit(context.Background(), p)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}

			var types []string
			for _, e := range repo.outbox {
				types = append(types, e.EventType)
				if e.AggregateID != p.WalletID {
					t.Errorf("%s aggregateId = %s, want the wallet", e.EventType, e.AggregateID)
				}
				if e.CorrelationID != result.Transaction.ID() {
					t.Errorf("%s correlationId = %s, want the transaction's id", e.EventType, e.CorrelationID)
				}
			}
			if !slices.Equal(types, test.want) {
				t.Errorf("outbox = %v, want %v", types, test.want)
			}
		})
	}
}

// A replay re-applies nothing, so it owes no second copy of the events (§9).
func TestReplayWritesNoFurtherEvents(t *testing.T) {
	service, repo, p := fixture(t, "100.00")
	if _, err := service.Submit(context.Background(), p); err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	written := len(repo.outbox)

	if _, err := service.Submit(context.Background(), p); err != nil {
		t.Fatalf("replay Submit() error = %v", err)
	}
	if len(repo.outbox) != written {
		t.Errorf("the replay wrote %d more events, want none", len(repo.outbox)-written)
	}
}
