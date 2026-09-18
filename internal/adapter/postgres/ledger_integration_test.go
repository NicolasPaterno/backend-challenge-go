//go:build integration

package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/app/walletapp"
)

// The ledger needs more rows than an opening produces, and only the wagering
// story writes them, so the fixture goes straight to SQL.
func insertEntry(t *testing.T, pool *pgxpool.Pool, walletID, playerID uuid.UUID, at time.Time) uuid.UUID {
	t.Helper()

	ctx := context.Background()
	transactionID := uuid.NewV7()
	const insertTransaction = `
		INSERT INTO wager_transactions (id, origin, kind, status, wallet_id, player_id, currency, amount_minor,
			provider_id, external_transaction_id, idempotency_key, payload_hash, round_id, game_id, created_at, updated_at)
		VALUES ($1, 'EXTERNAL', 'WIN', 'PROCESSED', $2, $3, 'BRL', 100,
			'provider-a', $5, $5, 'hash', 'round-1', 'game-1', $4, $4)`
	if _, err := pool.Exec(ctx, insertTransaction, transactionID, walletID, playerID, at, transactionID.String()); err != nil {
		t.Fatalf("insert transaction: %v", err)
	}

	entryID := uuid.NewV7()
	const insertEntry = `
		INSERT INTO wallet_ledger_entries (id, wallet_id, transaction_id, direction, currency,
			amount_minor, balance_before_minor, balance_after_minor, created_at)
		VALUES ($1, $2, $3, 'CREDIT', 'BRL', 100, 0, 100, $4)`
	if _, err := pool.Exec(ctx, insertEntry, entryID, walletID, transactionID, at); err != nil {
		t.Fatalf("insert ledger entry: %v", err)
	}
	return entryID
}

func TestLedgerPagesEveryEntryExactlyOnceWhileMoreArrive(t *testing.T) {
	ctx := context.Background()
	service, pool := newService(t)

	opened, err := service.Open(ctx, walletapp.OpenParams{PlayerID: uuid.NewV7(), InitialBalance: brl(t, "10.00")})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	at := time.Now().UTC()
	want := map[uuid.UUID]bool{}
	for i := range 24 {
		want[insertEntry(t, pool, opened.ID(), opened.PlayerID(), at.Add(time.Duration(i)*time.Millisecond))] = false
	}

	seen := map[uuid.UUID]int{}
	var after *walletapp.LedgerCursor
	for page := 0; ; page++ {
		if page > 10 {
			t.Fatal("paging did not terminate")
		}

		entries, err := service.Ledger(ctx, walletapp.LedgerParams{WalletID: opened.ID(), After: after, Limit: 10})
		if err != nil {
			t.Fatalf("Ledger() error = %v", err)
		}
		if len(entries) == 0 {
			break
		}
		for _, e := range entries {
			seen[e.ID()]++
		}

		last := entries[len(entries)-1]
		after = &walletapp.LedgerCursor{CreatedAt: last.CreatedAt(), ID: last.ID()}

		// a row arriving mid-scan must not shift a boundary. Only the first
		// two pages add one, so the scan still terminates.
		if page < 2 {
			insertEntry(t, pool, opened.ID(), opened.PlayerID(), at.Add(time.Duration(100+page)*time.Millisecond))
		}
	}

	for id := range want {
		if seen[id] != 1 {
			t.Errorf("entry %s read %d times, want exactly 1", id, seen[id])
		}
	}
	if seen[uuid.Nil()] != 0 {
		t.Error("read an entry with a nil id")
	}
}

func TestLedgerReportsAnUnknownWallet(t *testing.T) {
	service, _ := newService(t)

	_, err := service.Ledger(context.Background(), walletapp.LedgerParams{WalletID: uuid.NewV7()})
	if !errors.Is(err, walletapp.ErrNotFound) {
		t.Fatalf("Ledger() = %v, want ErrNotFound", err)
	}
}

func TestLedgerCapsTheLimit(t *testing.T) {
	ctx := context.Background()
	service, pool := newService(t)

	opened, err := service.Open(ctx, walletapp.OpenParams{PlayerID: uuid.NewV7(), InitialBalance: brl(t, "10.00")})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}

	at := time.Now().UTC()
	for i := range walletapp.LedgerMaxLimit + 5 {
		insertEntry(t, pool, opened.ID(), opened.PlayerID(), at.Add(time.Duration(i)*time.Millisecond))
	}

	entries, err := service.Ledger(ctx, walletapp.LedgerParams{WalletID: opened.ID(), Limit: 100_000})
	if err != nil {
		t.Fatalf("Ledger() error = %v", err)
	}
	if len(entries) != walletapp.LedgerMaxLimit {
		t.Errorf("entries = %d, want the %d cap", len(entries), walletapp.LedgerMaxLimit)
	}
}
