// Package postgres translates between the domain aggregates and SQL. It holds
// no business rule: every invariant it relies on is a constraint in the schema.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/app/walletapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
)

type WalletRepository struct {
	pool *pgxpool.Pool
}

func NewWalletRepository(pool *pgxpool.Pool) *WalletRepository {
	return &WalletRepository{pool: pool}
}

const (
	insertWallet = `
		INSERT INTO wallets (id, player_id, currency, balance_minor, version, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`

	// The external columns stay NULL, which is what
	// wager_transactions_origin_fields demands of an INTERNAL row (A.1).
	insertOpening = `
		INSERT INTO wager_transactions (
			id, origin, kind, status, wallet_id, player_id, currency, amount_minor,
			result_balance_minor, created_at, updated_at
		) VALUES ($1, 'INTERNAL', 'OPENING', $2, $3, $4, $5, $6, $7, $8, $9)`

	insertLedgerEntry = `
		INSERT INTO wallet_ledger_entries (
			id, wallet_id, transaction_id, direction, currency,
			amount_minor, balance_before_minor, balance_after_minor, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`

	selectWallet = `
		SELECT player_id, currency, balance_minor, version, created_at, updated_at
		FROM wallets WHERE id = $1`
)

// opening and entry are nil for a zero initial balance (§9).
func (r *WalletRepository) Open(ctx context.Context, w *wallet.Wallet, opening *wagering.WagerTransaction, entry *wallet.LedgerEntry) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, insertWallet,
			w.ID(), w.PlayerID(), w.Currency().String(), w.Balance().Minor(), w.Version(), w.CreatedAt(), w.UpdatedAt())
		if err != nil {
			if isUniqueViolation(err) {
				return walletapp.ErrAlreadyExists
			}
			return fmt.Errorf("insert wallet: %w", err)
		}

		if opening == nil {
			return nil
		}

		_, err = tx.Exec(ctx, insertOpening,
			opening.ID(), opening.Status().String(), opening.WalletID(), opening.PlayerID(),
			opening.Amount().Currency().String(), opening.Amount().Minor(),
			opening.ResultBalance().Minor(), opening.CreatedAt(), opening.UpdatedAt())
		if err != nil {
			if isUniqueViolation(err) {
				return walletapp.ErrAlreadyExists
			}
			return fmt.Errorf("insert opening: %w", err)
		}

		_, err = tx.Exec(ctx, insertLedgerEntry,
			entry.ID(), entry.WalletID(), entry.TransactionID(), entry.Direction().String(),
			entry.Amount().Currency().String(), entry.Amount().Minor(),
			entry.BalanceBefore().Minor(), entry.BalanceAfter().Minor(), entry.CreatedAt())
		if err != nil {
			return fmt.Errorf("insert ledger entry: %w", err)
		}
		return nil
	})
}

func (r *WalletRepository) ByID(ctx context.Context, id uuid.UUID) (*wallet.Wallet, error) {
	w, err := scanWallet(r.pool.QueryRow(ctx, selectWallet, id), id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, walletapp.ErrNotFound
	}
	return w, err
}

// Shared with the locking read of the wagering repository, which needs the same
// columns under FOR UPDATE. pgx.ErrNoRows is passed through: each caller names
// the absence in its own vocabulary.
func scanWallet(row pgx.Row, id uuid.UUID) (*wallet.Wallet, error) {
	var (
		playerID              uuid.UUID
		rawCurrency           string
		balanceMinor, version int64
		createdAt, updatedAt  time.Time
	)
	if err := row.Scan(&playerID, &rawCurrency, &balanceMinor, &version, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, err
		}
		return nil, fmt.Errorf("select wallet: %w", err)
	}

	currency, err := money.ParseCurrency(rawCurrency)
	if err != nil {
		return nil, fmt.Errorf("wallet %s: %w", id, err)
	}
	balance, err := money.FromMinor(balanceMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("wallet %s: %w", id, err)
	}
	return wallet.Rehydrate(id, playerID, balance, version, createdAt, updatedAt)
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == pgerrcode.UniqueViolation
}

const selectLedger = `
	SELECT id, transaction_id, direction, currency,
	       amount_minor, balance_before_minor, balance_after_minor, created_at
	FROM wallet_ledger_entries
	WHERE wallet_id = $1
	  AND ($2::timestamptz IS NULL OR (created_at, id) > ($2::timestamptz, $3::uuid))
	ORDER BY created_at, id
	LIMIT $4`

func (r *WalletRepository) Ledger(ctx context.Context, walletID uuid.UUID, after *walletapp.LedgerCursor, limit int) ([]*wallet.LedgerEntry, error) {
	var afterTime, afterID any
	if after != nil {
		afterTime, afterID = after.CreatedAt, after.ID
	}

	rows, err := r.pool.Query(ctx, selectLedger, walletID, afterTime, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("select ledger: %w", err)
	}
	defer rows.Close()

	var entries []*wallet.LedgerEntry
	for rows.Next() {
		var (
			id, transactionID                    uuid.UUID
			rawDirection, rawCurrency            string
			amountMinor, beforeMinor, afterMinor int64
			createdAt                            time.Time
		)
		if err := rows.Scan(&id, &transactionID, &rawDirection, &rawCurrency,
			&amountMinor, &beforeMinor, &afterMinor, &createdAt); err != nil {
			return nil, fmt.Errorf("scan ledger entry: %w", err)
		}

		currency, err := money.ParseCurrency(rawCurrency)
		if err != nil {
			return nil, fmt.Errorf("ledger entry %s: %w", id, err)
		}
		amount, err1 := money.FromMinor(amountMinor, currency)
		before, err2 := money.FromMinor(beforeMinor, currency)
		balanceAfter, err3 := money.FromMinor(afterMinor, currency)
		if err := errors.Join(err1, err2, err3); err != nil {
			return nil, fmt.Errorf("ledger entry %s: %w", id, err)
		}

		entry, err := wallet.NewLedgerEntry(id, walletID, transactionID,
			wallet.Direction(rawDirection), amount, before, balanceAfter, createdAt)
		if err != nil {
			return nil, fmt.Errorf("ledger entry %s: %w", id, err)
		}
		entries = append(entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read ledger: %w", err)
	}
	return entries, nil
}
