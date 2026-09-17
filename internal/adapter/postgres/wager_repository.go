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
	//
	// NO KEY UPDATE rather than UPDATE: the id never changes here, and the
	// weaker mode does not conflict with the FOR KEY SHARE that a foreign key
	// check takes on this row.
	lockWallet = selectWallet + " FOR NO KEY UPDATE"

	insertExternalTransaction = `
		INSERT INTO wager_transactions (
			id, origin, kind, status, wallet_id, player_id, currency, amount_minor,
			provider_id, external_transaction_id, idempotency_key, payload_hash,
			round_id, game_id, reference_external_transaction_id, reference_transaction_id,
			failure_code, result_balance_minor, created_at, updated_at
		) VALUES ($1, 'EXTERNAL', $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19)`

	// The two indexes §9's idempotency rests on, and the only ones whose
	// violation means "this operation already exists". The reversal index of
	// 0009 is deliberately absent: reaching it means the lock below failed to
	// serialise two reversals, which is a bug, not a replay.
	idempotencyKeyIndex = "wager_transactions_provider_key_unique"
	externalIDIndex     = "wager_transactions_provider_external_unique"

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
		switch {
		case isLockNotAvailable(err):
			return wageringapp.ErrWalletBusy
		case err != nil && !errors.Is(err, pgx.ErrNoRows):
			return err
		}

		var observed int64
		if w != nil {
			observed = w.Version()
		}

		// Read under the same transaction as the decision it feeds. No lock of
		// its own: a reversal only reaches PROCESSED after agreeing with its
		// reference on the wallet, so two that could collide already hold the
		// lock above. §8 puts the coordination per wallet and 0009's index is
		// what guarantees the rule in the schema (§5.3, §5.8).
		var ref *wageringapp.Reference
		if t.Kind().IsReversal() {
			if ref, err = reference(ctx, tx, t); err != nil {
				return err
			}
		}

		entry, outbox, err := decide(w, ref)
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
		case isUniqueViolationOn(err, idempotencyKeyIndex, externalIDIndex):
			return wageringapp.ErrDuplicate
		case err != nil:
			return fmt.Errorf("insert wager transaction: %w", err)
		}

		if entry == nil {
			return insertOutbox(ctx, tx, outbox)
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

		return insertOutbox(ctx, tx, outbox)
	})
}

const selectTransaction = `
	SELECT id, origin, kind, status, wallet_id, player_id, currency, amount_minor,
	       provider_id, external_transaction_id, idempotency_key, payload_hash,
	       round_id, game_id, reference_external_transaction_id, reference_transaction_id,
	       failure_code, result_balance_minor, created_at, updated_at
	FROM wager_transactions WHERE `

// A.8.1's rule as a query: any successful reversal spends the reference,
// whatever its kind. The partial unique index of 0009 is what holds it against
// a race the wallet lock does not cover.
const anySuccessfulReversal = `
	SELECT EXISTS (
		SELECT 1 FROM wager_transactions
		WHERE reference_transaction_id = $1
		  AND status = 'PROCESSED'
		  AND kind IN ('REFUND', 'ROLLBACK'))`

// reference resolves §7's (providerId, referenceExternalTransactionId). A nil
// result means nothing matched, which the use case waits on rather than
// refuses (A.8.2).
func reference(ctx context.Context, q querier, t *wagering.WagerTransaction) (*wageringapp.Reference, error) {
	found, err := one(ctx, q, selectTransaction+"provider_id = $1 AND external_transaction_id = $2",
		t.ProviderID(), t.ReferenceExternalTransactionID())
	if errors.Is(err, wageringapp.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	var reversed bool
	if err := q.QueryRow(ctx, anySuccessfulReversal, found.ID()).Scan(&reversed); err != nil {
		return nil, fmt.Errorf("check reversals of %s: %w", found.ID(), err)
	}
	return &wageringapp.Reference{Transaction: found, Reversed: reversed}, nil
}

func (r *WagerRepository) ByID(ctx context.Context, id uuid.UUID) (*wagering.WagerTransaction, error) {
	return one(ctx, r.pool, selectTransaction+"id = $1", id)
}

func (r *WagerRepository) ByIdempotencyKey(ctx context.Context, providerID, key string) (*wagering.WagerTransaction, error) {
	return one(ctx, r.pool, selectTransaction+"provider_id = $1 AND idempotency_key = $2", providerID, key)
}

func (r *WagerRepository) ByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wagering.WagerTransaction, error) {
	return one(ctx, r.pool, selectTransaction+"provider_id = $1 AND external_transaction_id = $2", providerID, externalTransactionID)
}

// querier is what the pool and an open transaction have in common, so a read
// runs inside Process's transaction or outside it without a second scanner.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

func one(ctx context.Context, q querier, query string, args ...any) (*wagering.WagerTransaction, error) {
	var (
		p                                                  wagering.RehydrateParams
		rawOrigin, rawKind, rawStatus, rawCurrency         string
		providerID, externalID, key, hash, roundID, gameID *string
		referenceExternalID, rawFailureCode                *string
		referenceID                                        *uuid.UUID
		amountMinor                                        int64
		resultBalanceMinor                                 *int64
	)
	err := q.QueryRow(ctx, query, args...).Scan(
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
