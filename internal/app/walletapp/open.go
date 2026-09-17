// Package walletapp holds the wallet use cases and declares the ports the
// adapters satisfy (§4).
package walletapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/events"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
)

var (
	ErrAlreadyExists = errors.New("walletapp: a wallet already exists for this player and currency")
	ErrNotFound      = errors.New("walletapp: wallet not found")
)

// Open takes the whole aggregate rather than a transaction handle: §9 requires
// the wallet, its OPENING, the credit entry and the outbox records to share one
// commit, and a single method makes a partial write unrepresentable.
type Repository interface {
	Open(ctx context.Context, w *wallet.Wallet, opening *wagering.WagerTransaction, entry *wallet.LedgerEntry, outbox []events.Envelope) error
	ByID(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error)
	Ledger(ctx context.Context, walletID uuid.UUID, after *LedgerCursor, limit int) ([]*wallet.LedgerEntry, error)
	// Reconcile reads the stored balance and the one rebuilt from the ledger in
	// a single consistent view, so a concurrent movement cannot show up as a
	// divergence (§9).
	Reconcile(ctx context.Context, walletID uuid.UUID) (stored, calculated money.Money, entries int, err error)
}

// A port so a test can make ids predictable without making them predictable in
// production (A.1 requirement 5).
type IDGenerator interface {
	NewID() uuid.UUID
}

type UUIDv7 struct{}

func (UUIDv7) NewID() uuid.UUID { return uuid.NewV7() }

type Service struct {
	repo Repository
	ids  IDGenerator
	now  func() time.Time
}

func NewService(repo Repository, ids IDGenerator) *Service {
	return &Service{repo: repo, ids: ids, now: func() time.Time { return time.Now().UTC() }}
}

type OpenParams struct {
	PlayerID       uuid.UUID
	InitialBalance money.Money
}

func (s *Service) Open(ctx context.Context, p OpenParams) (*wallet.Wallet, error) {
	now := s.now()

	w, err := wallet.New(s.ids.NewID(), p.PlayerID, p.InitialBalance, now)
	if err != nil {
		return nil, err
	}

	// §9: a zero opening credits nothing, so there is no OPENING, no entry and
	// none of the financial events.
	if p.InitialBalance.IsZero() {
		if err := s.repo.Open(ctx, w, nil, nil, nil); err != nil {
			return nil, err
		}
		return w, nil
	}

	openingID := s.ids.NewID()
	opening, err := wagering.NewInternalOpening(wagering.NewInternalOpeningParams{
		ID:       openingID,
		WalletID: w.ID(),
		PlayerID: w.PlayerID(),
		Amount:   p.InitialBalance,
		Now:      now,
	})
	if err != nil {
		return nil, err
	}
	if err := opening.MarkProcessed(p.InitialBalance, now); err != nil {
		return nil, err
	}

	entry, err := s.openingEntry(w, openingID, p.InitialBalance, now)
	if err != nil {
		return nil, err
	}

	// The OPENING's id stands in as correlationId while nothing carries one
	// (A.1); 16 replaces it with the request's.
	outbox := []events.Envelope{
		events.NewWagerTransactionProcessed(s.ids.NewID(), openingID, opening, now),
		events.NewWalletBalanceChanged(s.ids.NewID(), openingID, entry, w.Version(), now),
	}

	if err := s.repo.Open(ctx, w, opening, entry, outbox); err != nil {
		return nil, err
	}
	return w, nil
}

// The opening credit is recorded, not applied: §9 pins the wallet's version at
// 1 and wallet.New already carries the initial balance, so Credit here would
// move the balance twice. It is the one call to NewLedgerEntry outside
// rehydration; every later movement goes through Debit/Credit.
func (s *Service) openingEntry(w *wallet.Wallet, openingID uuid.UUID, amount money.Money, now time.Time) (*wallet.LedgerEntry, error) {
	zero, err := money.Zero(w.Currency())
	if err != nil {
		return nil, fmt.Errorf("zero of %s: %w", w.Currency(), err)
	}
	return wallet.NewLedgerEntry(s.ids.NewID(), w.ID(), openingID, wallet.DirectionCredit, amount, zero, amount, now)
}

func (s *Service) ByID(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	return s.repo.ByID(ctx, id)
}
