package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/app/wageringapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
)

type WagerRepository struct {
	pool *pgxpool.Pool
}

func NewWagerRepository(pool *pgxpool.Pool) *WagerRepository {
	return &WagerRepository{pool: pool}
}

const (
	// §8's coordination point. Per wallet, so independent wallets still run in
	// parallel; no advisory or table-wide lock is taken anywhere (§5.6).
	lockWallet = selectWallet + " FOR UPDATE"

	insertExternalTransaction = `
		INSERT INTO wager_transactions (
			id, origin, kind, status, wallet_id, player_id, currency, amount_minor,
			provider_id, external_transaction_id, idempotency_key, payload_hash,
			round_id, game_id, reference_external_transaction_id, reference_transaction_id,
			failure_code, result_balance_minor, created_at, updated_at
		) VALUES ($1, 'EXTERNAL', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`

	// Redundant under the lock above, and what still refuses a lost update if a
	// caller ever reaches this statement without it (§5.7).
	updateWalletBalance = `
		UPDATE wallets SET balance_minor = $2, version = $3, updated_at = $4
		WHERE id = $1 AND version = $5`
)

func (r *WagerRepository) Process(ctx context.Context, t *wagering.WagerTransaction, decide wageringapp.Decide) error {
	return pgx.BeginFunc(ctx, r.pool, func(tx pgx.Tx) error {
		// A wallet that does not exist is handed to decide as nil: the rejection
		// it produces is recorded like any other (§11).
		w, err := scanWallet(tx.QueryRow(ctx, lockWallet, t.WalletID()), t.WalletID())
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}

		var observed int64
		if w != nil {
			observed = w.Version()
		}

		entry, err := decide(w)
		if err != nil {
			return err
		}

		_, err = tx.Exec(ctx, insertExternalTransaction,
			t.ID(), t.Kind().String(), t.Status().String(), t.WalletID(), t.PlayerID(),
			t.Amount().Currency().String(), t.Amount().Minor(),
			t.ProviderID(), t.ExternalTransactionID(), t.IdempotencyKey(), t.PayloadHash(),
			t.RoundID(), t.GameID(),
			nullString(t.ReferenceExternalTransactionID()), nullUUID(t.ReferenceTransactionID()),
			nullString(t.FailureCode().String()), nullMinor(t.ResultBalance()),
			t.CreatedAt(), t.UpdatedAt())
		switch {
		case isUniqueViolation(err):
			return wageringapp.ErrDuplicate
		case err != nil:
			return fmt.Errorf("insert wager transaction: %w", err)
		}

		if entry == nil {
			return nil
		}

		_, err = tx.Exec(ctx, insertLedgerEntry,
			entry.ID(), entry.WalletID(), entry.TransactionID(), entry.Direction().String(),
			entry.Amount().Currency().String(), entry.Amount().Minor(),
			entry.BalanceBefore().Minor(), entry.BalanceAfter().Minor(), entry.CreatedAt())
		if err != nil {
			return fmt.Errorf("insert ledger entry: %w", err)
		}

		tag, err := tx.Exec(ctx, updateWalletBalance,
			w.ID(), w.Balance().Minor(), w.Version(), w.UpdatedAt(), observed)
		if err != nil {
			return fmt.Errorf("update wallet balance: %w", err)
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("wallet %s: %w", w.ID(), wageringapp.ErrConcurrentUpdate)
		}
		return nil
	})
}

const selectTransaction = `
	SELECT id, origin, kind, status, wallet_id, player_id, currency, amount_minor,
	       provider_id, external_transaction_id, idempotency_key, payload_hash,
	       round_id, game_id, reference_external_transaction_id, reference_transaction_id,
	       failure_code, result_balance_minor, created_at, updated_at
	FROM wager_transactions WHERE `

func (r *WagerRepository) ByID(ctx context.Context, id uuid.UUID) (*wagering.WagerTransaction, error) {
	return r.one(ctx, selectTransaction+"id = $1", id)
}

func (r *WagerRepository) ByIdempotencyKey(ctx context.Context, providerID, key string) (*wagering.WagerTransaction, error) {
	return r.one(ctx, selectTransaction+"provider_id = $1 AND idempotency_key = $2", providerID, key)
}

func (r *WagerRepository) ByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wagering.WagerTransaction, error) {
	return r.one(ctx, selectTransaction+"provider_id = $1 AND external_transaction_id = $2", providerID, externalTransactionID)
}

func (r *WagerRepository) one(ctx context.Context, query string, args ...any) (*wagering.WagerTransaction, error) {
	var (
		p                                                  wagering.RehydrateParams
		rawOrigin, rawKind, rawStatus, rawCurrency         string
		providerID, externalID, key, hash, roundID, gameID *string
		referenceExternalID, rawFailureCode                *string
		referenceID                                        *uuid.UUID
		amountMinor                                        int64
		resultBalanceMinor                                 *int64
	)
	err := r.pool.QueryRow(ctx, query, args...).Scan(
		&p.ID, &rawOrigin, &rawKind, &rawStatus, &p.WalletID, &p.PlayerID, &rawCurrency, &amountMinor,
		&providerID, &externalID, &key, &hash,
		&roundID, &gameID, &referenceExternalID, &referenceID,
		&rawFailureCode, &resultBalanceMinor, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, wageringapp.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("select wager transaction: %w", err)
	}

	currency, err := money.ParseCurrency(rawCurrency)
	if err != nil {
		return nil, fmt.Errorf("wager transaction %s: %w", p.ID, err)
	}
	p.Amount, err = money.FromMinor(amountMinor, currency)
	if err != nil {
		return nil, fmt.Errorf("wager transaction %s: %w", p.ID, err)
	}
	if resultBalanceMinor != nil {
		if p.ResultBalance, err = money.FromMinor(*resultBalanceMinor, currency); err != nil {
			return nil, fmt.Errorf("wager transaction %s: %w", p.ID, err)
		}
	}

	p.Origin = wagering.Origin(rawOrigin)
	p.Kind = wagering.Kind(rawKind)
	p.Status = wagering.Status(rawStatus)
	p.ProviderID = derefString(providerID)
	p.ExternalTransactionID = derefString(externalID)
	p.IdempotencyKey = derefString(key)
	p.PayloadHash = derefString(hash)
	p.RoundID = derefString(roundID)
	p.GameID = derefString(gameID)
	p.ReferenceExternalTransactionID = derefString(referenceExternalID)
	p.FailureCode = wagering.FailureCode(derefString(rawFailureCode))
	if referenceID != nil {
		p.ReferenceTransactionID = *referenceID
	}

	return wagering.Rehydrate(p)
}

// The origin CHECK reads NULL as "does not apply", so an empty domain string is
// stored as NULL rather than as a value no rule will ever read.
func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullUUID(id uuid.UUID) any {
	if id == uuid.Nil() {
		return nil
	}
	return id
}

func nullMinor(m money.Money) any {
	if !m.IsValid() {
		return nil
	}
	return m.Minor()
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
