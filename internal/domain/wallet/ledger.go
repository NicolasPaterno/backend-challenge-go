package wallet

import (
	"errors"
	"fmt"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
)

var (
	ErrInvalidDirection = errors.New("wallet: direction must be DEBIT or CREDIT")
	ErrLedgerEquation   = errors.New("wallet: balanceAfter does not match balanceBefore and the movement")
	ErrEmptyMovement    = errors.New("wallet: a ledger entry must move a non-zero amount")
)

type Direction string

const (
	DirectionDebit  Direction = "DEBIT"
	DirectionCredit Direction = "CREDIT"
)

func (d Direction) IsValid() bool { return d == DirectionDebit || d == DirectionCredit }

func (d Direction) String() string { return string(d) }

// LedgerEntry is immutable: a correction is a new entry, never an edit (§5.5).
type LedgerEntry struct {
	id            uuid.UUID
	walletID      uuid.UUID
	transactionID uuid.UUID
	direction     Direction
	amount        money.Money
	balanceBefore money.Money
	balanceAfter  money.Money
	createdAt     time.Time
}

// NewLedgerEntry refuses whatever the schema refuses, so an impossible entry
// fails as a domain error and not as a constraint violation at INSERT time.
func NewLedgerEntry(id, walletID, transactionID uuid.UUID, direction Direction, amount, balanceBefore, balanceAfter money.Money, createdAt time.Time) (*LedgerEntry, error) {
	for _, field := range []struct {
		name string
		id   uuid.UUID
	}{{"id", id}, {"walletId", walletID}, {"transactionId", transactionID}} {
		if field.id == uuid.Nil() {
			return nil, fmt.Errorf("%w: %s", ErrUninitialized, field.name)
		}
	}
	if !direction.IsValid() {
		return nil, fmt.Errorf("%w, got %q", ErrInvalidDirection, string(direction))
	}
	for _, field := range []struct {
		name   string
		amount money.Money
	}{{"amount", amount}, {"balanceBefore", balanceBefore}, {"balanceAfter", balanceAfter}} {
		if !field.amount.IsValid() {
			return nil, fmt.Errorf("%w: %s", ErrUninitialized, field.name)
		}
	}
	if amount.IsNegative() {
		return nil, fmt.Errorf("%w: entry amount %s", money.ErrNegativeAmount, amount)
	}
	// §6.4: a movement of nothing produces no entry.
	if amount.IsZero() {
		return nil, ErrEmptyMovement
	}
	if createdAt.IsZero() {
		return nil, fmt.Errorf("%w: createdAt", ErrUninitialized)
	}

	expected, err := apply(direction, balanceBefore, amount)
	if err != nil {
		return nil, err
	}
	difference, err := expected.Cmp(balanceAfter)
	if err != nil {
		return nil, err
	}
	if difference != 0 {
		return nil, fmt.Errorf("%w: %s %s %s is %s, got %s", ErrLedgerEquation, balanceBefore, direction, amount, expected, balanceAfter)
	}

	return &LedgerEntry{
		id:            id,
		walletID:      walletID,
		transactionID: transactionID,
		direction:     direction,
		amount:        amount,
		balanceBefore: balanceBefore,
		balanceAfter:  balanceAfter,
		createdAt:     createdAt,
	}, nil
}

func (e *LedgerEntry) ID() uuid.UUID { return e.id }

func (e *LedgerEntry) WalletID() uuid.UUID { return e.walletID }

func (e *LedgerEntry) TransactionID() uuid.UUID { return e.transactionID }

func (e *LedgerEntry) Direction() Direction { return e.direction }

func (e *LedgerEntry) Amount() money.Money { return e.amount }

func (e *LedgerEntry) BalanceBefore() money.Money { return e.balanceBefore }

func (e *LedgerEntry) BalanceAfter() money.Money { return e.balanceAfter }

func (e *LedgerEntry) CreatedAt() time.Time { return e.createdAt }

func apply(direction Direction, balance, amount money.Money) (money.Money, error) {
	switch direction {
	case DirectionDebit:
		return balance.Sub(amount)
	case DirectionCredit:
		return balance.Add(amount)
	default:
		return money.Money{}, fmt.Errorf("%w, got %q", ErrInvalidDirection, string(direction))
	}
}
