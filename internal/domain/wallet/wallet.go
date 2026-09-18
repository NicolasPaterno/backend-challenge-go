// Package wallet holds the financial aggregate: a balance that only moves
// together with the ledger entry recording the movement.
package wallet

import (
	"errors"
	"fmt"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
)

var (
	ErrUninitialized     = errors.New("wallet: value is uninitialised")
	ErrInsufficientFunds = errors.New("wallet: insufficient funds")
	ErrInvalidVersion    = errors.New("wallet: version must be at least 1")
)

type Wallet struct {
	id        uuid.UUID
	playerID  uuid.UUID
	balance   money.Money
	version   int64
	createdAt time.Time
	updatedAt time.Time
}

func New(id, playerID uuid.UUID, initialBalance money.Money, now time.Time) (*Wallet, error) {
	if err := validate(id, playerID, initialBalance, now); err != nil {
		return nil, err
	}
	if initialBalance.IsNegative() {
		return nil, fmt.Errorf("%w: initial balance %s", money.ErrNegativeAmount, initialBalance)
	}
	return &Wallet{
		id:        id,
		playerID:  playerID,
		balance:   initialBalance,
		version:   1,
		createdAt: now,
		updatedAt: now,
	}, nil
}

func Rehydrate(id, playerID uuid.UUID, balance money.Money, version int64, createdAt, updatedAt time.Time) (*Wallet, error) {
	if err := validate(id, playerID, balance, createdAt); err != nil {
		return nil, err
	}
	if updatedAt.IsZero() {
		return nil, fmt.Errorf("%w: updatedAt", ErrUninitialized)
	}
	if version < 1 {
		return nil, fmt.Errorf("%w, got %d", ErrInvalidVersion, version)
	}
	if balance.IsNegative() {
		return nil, fmt.Errorf("%w: balance %s", money.ErrNegativeAmount, balance)
	}
	return &Wallet{
		id:        id,
		playerID:  playerID,
		balance:   balance,
		version:   version,
		createdAt: createdAt,
		updatedAt: updatedAt,
	}, nil
}

func (w *Wallet) ID() uuid.UUID { return w.id }

func (w *Wallet) PlayerID() uuid.UUID { return w.playerID }

func (w *Wallet) Balance() money.Money { return w.balance }

func (w *Wallet) Currency() money.Currency { return w.balance.Currency() }

func (w *Wallet) Version() int64 { return w.version }

func (w *Wallet) CreatedAt() time.Time { return w.createdAt }

func (w *Wallet) UpdatedAt() time.Time { return w.updatedAt }

// A shortfall is refused, never overdrawn, and leaves the wallet untouched.
func (w *Wallet) Debit(entryID, transactionID uuid.UUID, amount money.Money, now time.Time) (*LedgerEntry, error) {
	return w.move(DirectionDebit, entryID, transactionID, amount, now)
}

func (w *Wallet) Credit(entryID, transactionID uuid.UUID, amount money.Money, now time.Time) (*LedgerEntry, error) {
	return w.move(DirectionCredit, entryID, transactionID, amount, now)
}

// The only path that writes the balance, so no caller can skip the entry or the
// version bump. A zero amount moves nothing: nil entry, version
// untouched, which is what a LOSS needs.
func (w *Wallet) move(direction Direction, entryID, transactionID uuid.UUID, amount money.Money, now time.Time) (*LedgerEntry, error) {
	if !amount.IsValid() {
		return nil, fmt.Errorf("%w: amount", ErrUninitialized)
	}
	if amount.IsNegative() {
		return nil, fmt.Errorf("%w: movement %s", money.ErrNegativeAmount, amount)
	}
	if now.IsZero() {
		return nil, fmt.Errorf("%w: now", ErrUninitialized)
	}

	before := w.balance
	after, err := apply(direction, before, amount)
	if err != nil {
		return nil, err
	}
	if after.IsNegative() {
		return nil, fmt.Errorf("%w: balance %s, debit %s", ErrInsufficientFunds, before, amount)
	}
	if amount.IsZero() {
		return nil, nil
	}

	entry, err := NewLedgerEntry(entryID, w.id, transactionID, direction, amount, before, after, now)
	if err != nil {
		return nil, err
	}
	w.balance = after
	w.version++
	w.updatedAt = now
	return entry, nil
}

func validate(id, playerID uuid.UUID, balance money.Money, now time.Time) error {
	if id == uuid.Nil() {
		return fmt.Errorf("%w: id", ErrUninitialized)
	}
	if playerID == uuid.Nil() {
		return fmt.Errorf("%w: playerId", ErrUninitialized)
	}
	if !balance.IsValid() {
		return fmt.Errorf("%w: balance", ErrUninitialized)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: createdAt", ErrUninitialized)
	}
	return nil
}
