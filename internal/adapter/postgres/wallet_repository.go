// Package postgres translates between the domain aggregates and SQL. It holds
// no business rule: every invariant it relies on is a constraint in the schema.
package postgres

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgerrcode"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/app/walletapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/events"
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

// opening, entry and outbox are nil for a zero initial balance.
func (r *WalletRepository) Open(ctx context.Context, w *wallet.Wallet, opening *wagering.WagerTransaction, entry *wallet.LedgerEntry, outbox []events.Envelope) error {
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

		return insertOutbox(ctx, tx, outbox)
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
	return hasCode(err, pgerrcode.UniqueViolation)
}

// isUniqueViolationOn names the indexes a caller knows how to answer for.
// wager_transactions carries more than one, and mapping all of them to a single
// meaning is what made a reversal race read as an idempotency conflict; an
// index not listed here is a bug and must surface as one.
func isUniqueViolationOn(err error, indexes ...string) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != pgerrcode.UniqueViolation {
		return false
	}
	return slices.Contains(indexes, pgErr.ConstraintName)
}

// The wallet is held by another writer for longer than DB_LOCK_TIMEOUT. Nothing
// was applied, so the caller may retry.
func isLockNotAvailable(err error) bool {
	return hasCode(err, pgerrcode.LockNotAvailable)
}

func hasCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == code
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

// One statement, so both sides are read from one snapshot: PostgreSQL takes it
// at statement start, which is what the brief asks of the consistent view. A wallet
// with no entries is the LEFT JOIN's zero, not a missing row.
const selectReconciliation = `
	SELECT w.currency, w.balance_minor,
	       COALESCE(SUM(CASE e.direction WHEN 'CREDIT' THEN e.amount_minor ELSE -e.amount_minor END), 0),
	       COUNT(e.id)
	FROM wallets w
	LEFT JOIN wallet_ledger_entries e ON e.wallet_id = w.id
	WHERE w.id = $1
	GROUP BY w.currency, w.balance_minor`

func (r *WalletRepository) Reconcile(ctx context.Context, walletID uuid.UUID) (stored, calculated money.Money, entries int, err error) {
	var (
		rawCurrency               string
		balanceMinor, ledgerMinor int64
	)
	err = r.pool.QueryRow(ctx, selectReconciliation, walletID).
		Scan(&rawCurrency, &balanceMinor, &ledgerMinor, &entries)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return stored, calculated, 0, walletapp.ErrNotFound
	case err != nil:
		return stored, calculated, 0, fmt.Errorf("select reconciliation: %w", err)
	}

	currency, err := money.ParseCurrency(rawCurrency)
	if err != nil {
		return stored, calculated, 0, fmt.Errorf("wallet %s: %w", walletID, err)
	}
	stored, err1 := money.FromMinor(balanceMinor, currency)
	calculated, err2 := money.FromMinor(ledgerMinor, currency)
	if err := errors.Join(err1, err2); err != nil {
		return stored, calculated, 0, fmt.Errorf("wallet %s: %w", walletID, err)
	}
	return stored, calculated, entries, nil
}
