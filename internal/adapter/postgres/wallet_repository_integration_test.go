//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"uuid"

	pgadapter "github.com/NicolasPaterno/backend-challenge-go/internal/adapter/postgres"
	"github.com/NicolasPaterno/backend-challenge-go/internal/app/walletapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
)

func newService(t *testing.T) (*walletapp.Service, *pgxpool.Pool) {
	t.Helper()

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testsupport.PostgresMigrated(t))
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	t.Cleanup(pool.Close)

	return walletapp.NewService(pgadapter.NewWalletRepository(pool), walletapp.UUIDv7{}), pool
}

func brl(t *testing.T, amount string) money.Money {
	t.Helper()

	parsed, err := money.Parse(amount, money.BRL)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", amount, err)
	}
	return parsed
}

func count(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()

	var n int
	if err := pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

func TestOpenCommitsWalletOpeningAndEntryTogether(t *testing.T) {
	ctx := context.Background()
	service, pool := newService(t)

	opened, err := service.Open(ctx, walletapp.OpenParams{PlayerID: uuid.NewV7(), InitialBalance: brl(t, "1000.00")})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if got := count(t, pool, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1 AND kind = 'OPENING' AND status = 'PROCESSED'`, opened.ID()); got != 1 {
		t.Errorf("OPENING rows = %d, want 1", got)
	}
	if got := count(t, pool, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1 AND direction = 'CREDIT' AND amount_minor = 100000`, opened.ID()); got != 1 {
		t.Errorf("credit entries = %d, want 1", got)
	}

	reread, err := service.ByID(ctx, opened.ID())
	if err != nil {
		t.Fatalf("ByID() error = %v", err)
	}
	if reread.Version() != 1 || reread.Balance().String() != "1000.00 BRL" {
		t.Errorf("wallet = %s at version %d, want 1000.00 BRL at version 1", reread.Balance(), reread.Version())
	}
}

func TestOpenWithZeroBalanceWritesOnlyTheWallet(t *testing.T) {
	ctx := context.Background()
	service, pool := newService(t)

	opened, err := service.Open(ctx, walletapp.OpenParams{PlayerID: uuid.NewV7(), InitialBalance: brl(t, "0.00")})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	if got := count(t, pool, `SELECT count(*) FROM wager_transactions WHERE wallet_id = $1`, opened.ID()); got != 0 {
		t.Errorf("transactions = %d, want 0", got)
	}
	if got := count(t, pool, `SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, opened.ID()); got != 0 {
		t.Errorf("ledger entries = %d, want 0", got)
	}
}

func TestOpenRejectsASecondWalletForTheSamePlayerAndCurrency(t *testing.T) {
	ctx := context.Background()
	service, pool := newService(t)

	playerID := uuid.NewV7()
	params := walletapp.OpenParams{PlayerID: playerID, InitialBalance: brl(t, "10.00")}
	if _, err := service.Open(ctx, params); err != nil {
		t.Fatalf("first Open() error = %v", err)
	}

	if _, err := service.Open(ctx, params); !errors.Is(err, walletapp.ErrAlreadyExists) {
		t.Fatalf("second Open() = %v, want ErrAlreadyExists", err)
	}
	if got := count(t, pool, `SELECT count(*) FROM wallets WHERE player_id = $1`, playerID); got != 1 {
		t.Errorf("wallets = %d, want 1", got)
	}
}

func TestByIDReportsAnUnknownWallet(t *testing.T) {
	service, _ := newService(t)

	if _, err := service.ByID(context.Background(), uuid.NewV7()); !errors.Is(err, walletapp.ErrNotFound) {
		t.Fatalf("ByID() = %v, want ErrNotFound", err)
	}
}

func TestSchemaEnforcesTheFinancialInvariants(t *testing.T) {
	ctx := context.Background()
	service, pool := newService(t)

	opened, err := service.Open(ctx, walletapp.OpenParams{PlayerID: uuid.NewV7(), InitialBalance: brl(t, "10.00")})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	var openingID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM wager_transactions WHERE wallet_id = $1`, opened.ID()).Scan(&openingID); err != nil {
		t.Fatalf("read opening id: %v", err)
	}

	tests := []struct {
		name  string
		query string
		args  []any
	}{
		{ // A.1 requirement 5
			"a second OPENING for the same wallet",
			`INSERT INTO wager_transactions (id, origin, kind, status, wallet_id, player_id, currency, amount_minor, created_at, updated_at)
			 VALUES ($1, 'INTERNAL', 'OPENING', 'PROCESSED', $2, $3, 'BRL', 500, now(), now())`,
			[]any{uuid.NewV7(), opened.ID(), opened.PlayerID()},
		},
		{
			"a second entry for the same (wallet, transaction)",
			`INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, currency, amount_minor, balance_before_minor, balance_after_minor, created_at)
			 VALUES ($1, $2, $3, 'CREDIT', 'BRL', 100, 1000, 1100, now())`,
			[]any{uuid.NewV7(), opened.ID(), openingID},
		},
		{
			"editing a ledger entry",
			`UPDATE wallet_ledger_entries SET amount_minor = 1 WHERE wallet_id = $1`,
			[]any{opened.ID()},
		},
		{
			"deleting a ledger entry",
			`DELETE FROM wallet_ledger_entries WHERE wallet_id = $1`,
			[]any{opened.ID()},
		},
		{
			"a negative balance",
			`UPDATE wallets SET balance_minor = -1 WHERE id = $1`,
			[]any{opened.ID()},
		},
		{ // balanceAfter must match balanceBefore ± amount
			"an entry whose arithmetic does not hold",
			`INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, currency, amount_minor, balance_before_minor, balance_after_minor, created_at)
			 VALUES ($1, $2, $3, 'CREDIT', 'BRL', 100, 1000, 9999, now())`,
			[]any{uuid.NewV7(), opened.ID(), uuid.NewV7()},
		},
		{ // A.1 requirement 3
			"an INTERNAL row carrying provider metadata",
			`INSERT INTO wager_transactions (id, origin, kind, status, wallet_id, player_id, currency, amount_minor, provider_id, created_at, updated_at)
			 VALUES ($1, 'INTERNAL', 'OPENING', 'PROCESSED', $2, $3, 'BRL', 500, 'provider-a', now(), now())`,
			[]any{uuid.NewV7(), opened.ID(), opened.PlayerID()},
		},
		{ // A.1 requirement 3
			"an EXTERNAL row missing its provider metadata",
			`INSERT INTO wager_transactions (id, origin, kind, status, wallet_id, player_id, currency, amount_minor, created_at, updated_at)
			 VALUES ($1, 'EXTERNAL', 'BET', 'PENDING', $2, $3, 'BRL', 500, now(), now())`,
			[]any{uuid.NewV7(), opened.ID(), opened.PlayerID()},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, test.query, test.args...); err == nil {
				t.Fatalf("%s succeeded, want the database to refuse it", test.name)
			}
		})
	}
}

func TestDistinctWalletsOpenInParallel(t *testing.T) {
	service, _ := newService(t)

	// no global lock, so independent wallets never wait on each other.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	errs := make(chan error, 8)
	for range cap(errs) {
		go func() {
			_, err := service.Open(ctx, walletapp.OpenParams{PlayerID: uuid.NewV7(), InitialBalance: brl(t, "50.00")})
			errs <- err
		}()
	}
	for range cap(errs) {
		if err := <-errs; err != nil {
			t.Errorf("parallel Open() error = %v", err)
		}
	}
}
