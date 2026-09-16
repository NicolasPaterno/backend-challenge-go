package wallet_test

import (
	"errors"
	"testing"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
)

var (
	walletID  = uuid.MustParse("0192f291-27dd-7d3f-8071-5f8685deef37")
	playerID  = uuid.MustParse("0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1")
	entryID   = uuid.MustParse("0192f298-345e-7e38-af88-e43f851a819d")
	txID      = uuid.MustParse("0192f299-1111-7e38-af88-e43f851a8200")
	createdAt = time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	movedAt   = createdAt.Add(time.Hour)
)

func brl(t *testing.T, amount string) money.Money {
	t.Helper()
	m, err := money.Parse(amount, money.BRL)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", amount, err)
	}
	return m
}

func newWallet(t *testing.T, balance string) *wallet.Wallet {
	t.Helper()
	w, err := wallet.New(walletID, playerID, brl(t, balance), createdAt)
	if err != nil {
		t.Fatalf("New(%q) error = %v", balance, err)
	}
	return w
}

func assertBalance(t *testing.T, w *wallet.Wallet, want string) {
	t.Helper()
	difference, err := w.Balance().Cmp(brl(t, want))
	if err != nil || difference != 0 {
		t.Errorf("Balance() = %s, want %s (cmp error %v)", w.Balance(), want, err)
	}
}

func TestNewStartsAtVersionOne(t *testing.T) {
	w := newWallet(t, "1000.00")

	if w.Version() != 1 {
		t.Errorf("Version() = %d, want 1", w.Version())
	}
	if w.ID() != walletID || w.PlayerID() != playerID {
		t.Errorf("ID()/PlayerID() = %s/%s, want %s/%s", w.ID(), w.PlayerID(), walletID, playerID)
	}
	if w.Currency() != money.BRL {
		t.Errorf("Currency() = %q, want %q", w.Currency(), money.BRL)
	}
	if !w.CreatedAt().Equal(createdAt) || !w.UpdatedAt().Equal(createdAt) {
		t.Errorf("timestamps = %s/%s, want %s", w.CreatedAt(), w.UpdatedAt(), createdAt)
	}
}

func TestNewRejectsUninitialisedValues(t *testing.T) {
	valid := brl(t, "10.00")
	negative, err := money.FromMinor(-1, money.BRL)
	if err != nil {
		t.Fatalf("FromMinor error = %v", err)
	}

	tests := map[string]struct {
		id, player uuid.UUID
		balance    money.Money
		now        time.Time
		want       error
	}{
		"nil id":           {uuid.Nil(), playerID, valid, createdAt, wallet.ErrUninitialized},
		"nil player":       {walletID, uuid.Nil(), valid, createdAt, wallet.ErrUninitialized},
		"zero value money": {walletID, playerID, money.Money{}, createdAt, wallet.ErrUninitialized},
		"zero time":        {walletID, playerID, valid, time.Time{}, wallet.ErrUninitialized},
		"negative balance": {walletID, playerID, negative, createdAt, money.ErrNegativeAmount},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := wallet.New(test.id, test.player, test.balance, test.now); !errors.Is(err, test.want) {
				t.Errorf("New() error = %v, want %v", err, test.want)
			}
		})
	}
}

// The guard tests IsValid, never IsZero: a zero opening balance is valid (§9).
func TestNewAcceptsZeroBalance(t *testing.T) {
	w := newWallet(t, "0.00")

	assertBalance(t, w, "0.00")
	if w.Version() != 1 {
		t.Errorf("Version() = %d, want 1", w.Version())
	}
}

func TestDebitMovesBalanceAndEmitsEntry(t *testing.T) {
	w := newWallet(t, "1000.00")

	entry, err := w.Debit(entryID, txID, brl(t, "25.00"), movedAt)
	if err != nil {
		t.Fatalf("Debit() error = %v", err)
	}

	assertBalance(t, w, "975.00")
	if w.Version() != 2 {
		t.Errorf("Version() = %d, want 2", w.Version())
	}
	if !w.UpdatedAt().Equal(movedAt) {
		t.Errorf("UpdatedAt() = %s, want %s", w.UpdatedAt(), movedAt)
	}
	if entry.Direction() != wallet.DirectionDebit {
		t.Errorf("Direction() = %q, want %q", entry.Direction(), wallet.DirectionDebit)
	}
	if entry.WalletID() != walletID || entry.TransactionID() != txID {
		t.Errorf("entry ids = %s/%s, want %s/%s", entry.WalletID(), entry.TransactionID(), walletID, txID)
	}
	if entry.BalanceBefore().Minor() != 100000 || entry.BalanceAfter().Minor() != 97500 {
		t.Errorf("entry balances = %s/%s, want 1000.00/975.00", entry.BalanceBefore(), entry.BalanceAfter())
	}
}

func TestCreditMovesBalanceAndEmitsEntry(t *testing.T) {
	w := newWallet(t, "10.00")

	entry, err := w.Credit(entryID, txID, brl(t, "15.50"), movedAt)
	if err != nil {
		t.Fatalf("Credit() error = %v", err)
	}

	assertBalance(t, w, "25.50")
	if w.Version() != 2 {
		t.Errorf("Version() = %d, want 2", w.Version())
	}
	if entry.Direction() != wallet.DirectionCredit {
		t.Errorf("Direction() = %q, want %q", entry.Direction(), wallet.DirectionCredit)
	}
}

func TestMovementsIncrementVersionByExactlyOne(t *testing.T) {
	w := newWallet(t, "100.00")

	for i := int64(1); i <= 3; i++ {
		if _, err := w.Debit(uuid.New(), uuid.New(), brl(t, "10.00"), movedAt); err != nil {
			t.Fatalf("Debit() error = %v", err)
		}
		if want := 1 + i; w.Version() != want {
			t.Fatalf("Version() = %d, want %d", w.Version(), want)
		}
	}
}

func TestDebitBeyondBalanceIsRejected(t *testing.T) {
	w := newWallet(t, "100.00")

	entry, err := w.Debit(entryID, txID, brl(t, "100.01"), movedAt)
	if !errors.Is(err, wallet.ErrInsufficientFunds) {
		t.Fatalf("Debit() error = %v, want ErrInsufficientFunds", err)
	}
	if entry != nil {
		t.Errorf("entry = %v, want nil", entry)
	}
	assertBalance(t, w, "100.00")
	if w.Version() != 1 {
		t.Errorf("Version() = %d, want 1 — a rejected debit must not bump the version", w.Version())
	}
}

func TestDebitToExactlyZeroIsAllowed(t *testing.T) {
	w := newWallet(t, "100.00")

	if _, err := w.Debit(entryID, txID, brl(t, "100.00"), movedAt); err != nil {
		t.Fatalf("Debit() error = %v", err)
	}
	assertBalance(t, w, "0.00")
}

func TestMovementCurrencyMustMatchWallet(t *testing.T) {
	euro, err := money.Parse("1.00", money.EUR)
	if err != nil {
		t.Fatalf("Parse error = %v", err)
	}

	for name, move := range map[string]func(*wallet.Wallet) (*wallet.LedgerEntry, error){
		"debit":  func(w *wallet.Wallet) (*wallet.LedgerEntry, error) { return w.Debit(entryID, txID, euro, movedAt) },
		"credit": func(w *wallet.Wallet) (*wallet.LedgerEntry, error) { return w.Credit(entryID, txID, euro, movedAt) },
	} {
		t.Run(name, func(t *testing.T) {
			w := newWallet(t, "100.00")
			if _, err := move(w); !errors.Is(err, money.ErrCurrencyMismatch) {
				t.Fatalf("error = %v, want ErrCurrencyMismatch", err)
			}
			assertBalance(t, w, "100.00")
			if w.Version() != 1 {
				t.Errorf("Version() = %d, want 1", w.Version())
			}
		})
	}
}

// §7, LOSS row.
func TestZeroMovementChangesNothing(t *testing.T) {
	for name, move := range map[string]func(*wallet.Wallet, money.Money) (*wallet.LedgerEntry, error){
		"debit": func(w *wallet.Wallet, m money.Money) (*wallet.LedgerEntry, error) {
			return w.Debit(entryID, txID, m, movedAt)
		},
		"credit": func(w *wallet.Wallet, m money.Money) (*wallet.LedgerEntry, error) {
			return w.Credit(entryID, txID, m, movedAt)
		},
	} {
		t.Run(name, func(t *testing.T) {
			w := newWallet(t, "100.00")

			// "0" and "0.00" are the same amount after normalisation (A.3.1).
			for _, spelling := range []string{"0.00", "0"} {
				entry, err := move(w, brl(t, spelling))
				if err != nil {
					t.Fatalf("move(%q) error = %v", spelling, err)
				}
				if entry != nil {
					t.Errorf("move(%q) entry = %v, want nil", spelling, entry)
				}
			}
			assertBalance(t, w, "100.00")
			if w.Version() != 1 {
				t.Errorf("Version() = %d, want 1", w.Version())
			}
			if !w.UpdatedAt().Equal(createdAt) {
				t.Errorf("UpdatedAt() = %s, want it untouched at %s", w.UpdatedAt(), createdAt)
			}
		})
	}
}

func TestMovementRejectsUninitialisedValues(t *testing.T) {
	negative, err := money.FromMinor(-500, money.BRL)
	if err != nil {
		t.Fatalf("FromMinor error = %v", err)
	}

	tests := map[string]struct {
		amount money.Money
		now    time.Time
		want   error
	}{
		"zero value money": {money.Money{}, movedAt, wallet.ErrUninitialized},
		"zero time":        {brl(t, "1.00"), time.Time{}, wallet.ErrUninitialized},
		"negative amount":  {negative, movedAt, money.ErrNegativeAmount},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			w := newWallet(t, "100.00")
			if _, err := w.Credit(entryID, txID, test.amount, test.now); !errors.Is(err, test.want) {
				t.Errorf("Credit() error = %v, want %v", err, test.want)
			}
			if w.Version() != 1 {
				t.Errorf("Version() = %d, want 1", w.Version())
			}
		})
	}
}

func TestMovementRejectsNilEntryIdentifiers(t *testing.T) {
	w := newWallet(t, "100.00")

	if _, err := w.Debit(uuid.Nil(), txID, brl(t, "1.00"), movedAt); !errors.Is(err, wallet.ErrUninitialized) {
		t.Errorf("Debit() error = %v, want ErrUninitialized", err)
	}
	if _, err := w.Debit(entryID, uuid.Nil(), brl(t, "1.00"), movedAt); !errors.Is(err, wallet.ErrUninitialized) {
		t.Errorf("Debit() error = %v, want ErrUninitialized", err)
	}
	assertBalance(t, w, "100.00")
	if w.Version() != 1 {
		t.Errorf("Version() = %d, want 1 — a rejected movement must not bump the version", w.Version())
	}
}

func TestRehydrateReplaysNothing(t *testing.T) {
	updatedAt := createdAt.Add(48 * time.Hour)

	w, err := wallet.Rehydrate(walletID, playerID, brl(t, "975.00"), 7, createdAt, updatedAt)
	if err != nil {
		t.Fatalf("Rehydrate() error = %v", err)
	}

	assertBalance(t, w, "975.00")
	if w.Version() != 7 {
		t.Errorf("Version() = %d, want 7 — rehydration must not re-apply a movement", w.Version())
	}
	if !w.CreatedAt().Equal(createdAt) || !w.UpdatedAt().Equal(updatedAt) {
		t.Errorf("timestamps = %s/%s, want %s/%s", w.CreatedAt(), w.UpdatedAt(), createdAt, updatedAt)
	}
}

func TestRehydrateRejectsInvalidState(t *testing.T) {
	valid := brl(t, "10.00")
	negative, err := money.FromMinor(-1, money.BRL)
	if err != nil {
		t.Fatalf("FromMinor error = %v", err)
	}

	tests := map[string]struct {
		balance              money.Money
		version              int64
		createdAt, updatedAt time.Time
		want                 error
	}{
		"version zero":     {valid, 0, createdAt, createdAt, wallet.ErrInvalidVersion},
		"negative version": {valid, -1, createdAt, createdAt, wallet.ErrInvalidVersion},
		"zero createdAt":   {valid, 1, time.Time{}, createdAt, wallet.ErrUninitialized},
		"zero updatedAt":   {valid, 1, createdAt, time.Time{}, wallet.ErrUninitialized},
		"invalid money":    {money.Money{}, 1, createdAt, createdAt, wallet.ErrUninitialized},
		"negative balance": {negative, 1, createdAt, createdAt, money.ErrNegativeAmount},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := wallet.Rehydrate(walletID, playerID, test.balance, test.version, test.createdAt, test.updatedAt)
			if !errors.Is(err, test.want) {
				t.Errorf("Rehydrate() error = %v, want %v", err, test.want)
			}
		})
	}
}
