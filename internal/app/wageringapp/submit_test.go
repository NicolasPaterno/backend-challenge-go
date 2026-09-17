package wageringapp

import (
	"context"
	"errors"
	"testing"
	"time"

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

// fakeRepo stands in for the two unique indexes and the locked wallet row. The
// real guarantees are exercised against PostgreSQL in cmd/api.
type fakeRepo struct {
	wallet     *wallet.Wallet
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
	if r.wallet == nil || r.wallet.ID() != t.WalletID() {
		return ErrWalletNotFound
	}
	if _, err := decide(r.wallet); err != nil {
		return err
	}
	r.byKey[t.ProviderID()+"|"+t.IdempotencyKey()] = t
	r.byExternal[t.ProviderID()+"|"+t.ExternalTransactionID()] = t
	return nil
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

func TestSubmitRejectsAnotherPlayersWallet(t *testing.T) {
	service, _, p := fixture(t, "100.00")
	p.PlayerID = uuid.NewV7()

	result, err := service.Submit(context.Background(), p)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got := result.Transaction.FailureCode(); got != wagering.FailureWalletNotFound {
		t.Errorf("failureCode = %s, want WALLET_NOT_FOUND", got)
	}
}

func TestSubmitRefusesOpeningAndUnhandledKinds(t *testing.T) {
	service, _, p := fixture(t, "100.00")

	for _, kind := range []wagering.Kind{wagering.KindOpening, wagering.KindWin, wagering.KindLoss} {
		p.Kind = kind
		if _, err := service.Submit(context.Background(), p); err == nil {
			t.Errorf("Submit(%s) succeeded; only BET is handled here", kind)
		}
	}
}
