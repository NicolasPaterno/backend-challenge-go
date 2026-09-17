// Package wageringapp holds the use case both transports share: one
// submission path, so HTTP and SQS get the same financial guarantees (§10).
package wageringapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
)

var (
	// ErrDuplicate covers both idempotency indexes; Submit re-reads to learn
	// which one fired.
	ErrDuplicate = errors.New("wageringapp: a transaction with this identity already exists")

	ErrPayloadConflict    = errors.New("wageringapp: the idempotency key was reused with different content")
	ErrExternalIDConflict = errors.New("wageringapp: the operation was already submitted under another idempotency key")
	ErrConcurrentUpdate   = errors.New("wageringapp: the wallet changed under the update")
	ErrNotFound           = errors.New("wageringapp: transaction not found")
	ErrUnsupportedKind    = errors.New("wageringapp: kind is not handled yet")
)

// Decide runs inside the repository's SQL transaction with the wallet row
// already locked. It returns nil when the operation moved no money, and settles
// the transaction's own state before the repository writes it.
//
// w is nil when no wallet carries that id. That is a rejection like any other
// and is still recorded, because §11 owes every rejection an event.
type Decide func(w *wallet.Wallet) (*wallet.LedgerEntry, error)

// Process owns the whole commit rather than handing out a transaction handle: a
// caller holding one could commit half of it (§5.3).
type Repository interface {
	Process(ctx context.Context, t *wagering.WagerTransaction, decide Decide) error
	ByID(ctx context.Context, id uuid.UUID) (*wagering.WagerTransaction, error)
	ByIdempotencyKey(ctx context.Context, providerID, key string) (*wagering.WagerTransaction, error)
	ByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wagering.WagerTransaction, error)
}

// Duplicated from walletapp so the two use case packages share no import (§4).
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

type SubmitParams struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       uuid.UUID
	WalletID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           wagering.Kind
	Money                          money.Money
	ReferenceExternalTransactionID string
}

type Result struct {
	Transaction *wagering.WagerTransaction
	Replay      bool
}

func (s *Service) Submit(ctx context.Context, p SubmitParams) (Result, error) {
	now := s.now()
	hash := PayloadHash(p)

	t, err := wagering.NewExternal(wagering.NewExternalParams{
		ID:                             s.ids.NewID(),
		ProviderID:                     p.ProviderID,
		ExternalTransactionID:          p.ExternalTransactionID,
		IdempotencyKey:                 p.IdempotencyKey,
		PayloadHash:                    hash,
		WalletID:                       p.WalletID,
		PlayerID:                       p.PlayerID,
		RoundID:                        p.RoundID,
		GameID:                         p.GameID,
		Kind:                           p.Kind,
		Amount:                         p.Money,
		ReferenceExternalTransactionID: p.ReferenceExternalTransactionID,
		Now:                            now,
	})
	if err != nil {
		return Result{}, err
	}

	// After the constructor, so OPENING is refused by the domain rule that owns
	// it (A.1). 09 adds WIN and LOSS, 12 the reversals.
	if t.Kind() != wagering.KindBet {
		return Result{}, fmt.Errorf("%w: %s", ErrUnsupportedKind, t.Kind())
	}

	// No pre-read: the unique violation is the only duplicate check that also
	// holds against a submission racing this one in another process.
	err = s.repo.Process(ctx, t, s.decide(t, now))
	switch {
	case err == nil:
		return Result{Transaction: t}, nil
	case errors.Is(err, ErrDuplicate):
		return s.replay(ctx, p, hash)
	default:
		return Result{}, err
	}
}

// replay says what the unique violation meant. No record under this key means
// the other index fired: the operation already exists under a second key, which
// §9 forbids.
func (s *Service) replay(ctx context.Context, p SubmitParams, hash string) (Result, error) {
	stored, err := s.repo.ByIdempotencyKey(ctx, p.ProviderID, p.IdempotencyKey)
	switch {
	case errors.Is(err, ErrNotFound):
		return Result{}, ErrExternalIDConflict
	case err != nil:
		return Result{}, err
	case stored.PayloadHash() != hash:
		return Result{}, ErrPayloadConflict
	}
	return Result{Transaction: stored, Replay: true}, nil
}

// The balance reported here is the one a replay returns, even after later
// movements (§9).
func (s *Service) decide(t *wagering.WagerTransaction, now time.Time) Decide {
	return func(w *wallet.Wallet) (*wallet.LedgerEntry, error) {
		// One code for "no such wallet" and "not this player's wallet": telling
		// them apart would enumerate wallets (§2). Neither has a balance to
		// report, so the rejection carries the zero Money (04 §7).
		if w == nil || w.PlayerID() != t.PlayerID() {
			return nil, t.Reject(wagering.FailureWalletNotFound, money.Money{}, now)
		}
		if w.Currency() != t.Amount().Currency() {
			return nil, t.Reject(wagering.FailureCurrencyMismatch, w.Balance(), now)
		}

		entry, err := w.Debit(s.ids.NewID(), t.ID(), t.Amount(), now)
		switch {
		case errors.Is(err, wallet.ErrInsufficientFunds):
			return nil, t.Reject(wagering.FailureInsufficientFunds, w.Balance(), now)
		case err != nil:
			return nil, err
		}
		return entry, t.MarkProcessed(w.Balance(), now)
	}
}

func (s *Service) ByID(ctx context.Context, id uuid.UUID) (*wagering.WagerTransaction, error) {
	return s.repo.ByID(ctx, id)
}

func (s *Service) ByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wagering.WagerTransaction, error) {
	return s.repo.ByExternalID(ctx, providerID, externalTransactionID)
}
