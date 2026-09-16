//go:build integration

package migrations_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
	"github.com/NicolasPaterno/backend-challenge-go/migrations"
)

func TestMigrationsApplyAndRollBack(t *testing.T) {
	ctx := context.Background()
	databaseURL := testsupport.Postgres(t)

	assertVersion := func(want uint) {
		t.Helper()
		version, dirty, err := migrations.Version(databaseURL)
		if err != nil {
			t.Fatalf("Version() error = %v", err)
		}
		if dirty {
			t.Fatalf("schema is dirty at version %d", version)
		}
		if version != want {
			t.Fatalf("schema version = %d, want %d", version, want)
		}
	}

	assertVersion(0)

	if err := migrations.Up(databaseURL); err != nil {
		t.Fatalf("Up() error = %v", err)
	}
	assertVersion(4)

	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	var installed bool
	const hasExtension = `SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'pgcrypto')`
	if err := conn.QueryRow(ctx, hasExtension).Scan(&installed); err != nil {
		t.Fatalf("query pg_extension: %v", err)
	}
	if !installed {
		t.Error("pgcrypto is not installed after Up()")
	}

	for _, table := range []string{"wallets", "wager_transactions", "wallet_ledger_entries"} {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT to_regclass($1) IS NOT NULL`, table).Scan(&exists); err != nil {
			t.Fatalf("query to_regclass(%s): %v", table, err)
		}
		if !exists {
			t.Errorf("table %s is missing after Up()", table)
		}
	}

	if err := migrations.Up(databaseURL); err != nil {
		t.Fatalf("second Up() must be a no-op, got %v", err)
	}

	if err := migrations.Down(databaseURL); err != nil {
		t.Fatalf("Down() error = %v", err)
	}
	assertVersion(0)
}
