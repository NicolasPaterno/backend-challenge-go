// Package wageringapp holds the use case both transports share: one
// submission path, so HTTP and SQS get the same financial guarantees (§10).
package wageringapp

import (
	"context"
	"errors"
	"fmt"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/events"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
)

var (
	// ErrDuplicate covers both idempotency indexes; Submit re-reads to learn
	// which one fired.
	ErrDuplicate = errors.New("wageringapp: a transaction with this identity already exists")

	ErrPayloadConflict    = errors.New("wageringapp: the idempotency key was reused with different content")
	ErrExternalIDConflict = errors.New("wageringapp: the operation was already submitted under another idempotency key")
	ErrConcurrentUpdate   = errors.New("wageringapp: the wallet changed under the update")
	ErrWalletBusy         = errors.New("wageringapp: the wallet is held by another writer")
	ErrNotFound           = errors.New("wageringapp: transaction not found")

	// ErrDuplicateMessage is the inbox's own uniqueness firing: this consumer
	// already handled a message with this id (§6.5).
	ErrDuplicateMessage = errors.New("wageringapp: this message was already handled")
	// ErrMessageConflict is the same message id carrying different content, which
	// §10 requires be detected on a redelivery.
	ErrMessageConflict = errors.New("wageringapp: the message id was reused with different content")
	ErrUnsupportedKind = errors.New("wageringapp: kind is not handled yet")
)

// Reference is the operation a REFUND or ROLLBACK undoes, read by the
// repository inside the same transaction so the decision and the row it rests
// on cannot drift apart (§7).
type Reference struct {
	Transaction *wagering.WagerTransaction
	// Reversed reports that a REFUND or ROLLBACK over this reference already
	// succeeded. One successful reversal per reference, whatever its kind:
	// allowing one of each would return the same debit twice (A.8.1).
	Reversed bool
}

// Decide runs inside the repository's SQL transaction with the wallet row
// already locked. It settles the transaction's own state, and returns the
// movement — nil when the operation moved no money — together with the events
// that outcome owes, for the repository to write in the same commit (§5.4).
//
// w is nil when no wallet carries that id. That is a rejection like any other
// and is still recorded, because §11 owes every rejection an event.
//
// ref is nil for a kind that needs none, and for a reversal whose reference has
// not arrived.
type Decide func(w *wallet.Wallet, ref *Reference) (*wallet.LedgerEntry, []events.Envelope, error)

// Process owns the whole commit rather than handing out a transaction handle: a
// caller holding one could commit half of it (§5.3).
type Repository interface {
	// inbox is zero for an HTTP submission; when it is not, its row is written
	// in this same transaction (§6.5).
	Process(ctx context.Context, t *wagering.WagerTransaction, inbox Inbox, decide Decide) error
	ByID(ctx context.Context, id uuid.UUID) (*wagering.WagerTransaction, error)
	ByIdempotencyKey(ctx context.Context, providerID, key string) (*wagering.WagerTransaction, error)
	ByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wagering.WagerTransaction, error)

	// MessageHash is the payload hash stored when this consumer handled the
	// message, which §10 requires be verified on a redelivery.
	MessageHash(ctx context.Context, messageID string) (string, error)

	// DuePendingReferences lists the waiting reversals whose next attempt has
	// come round, oldest first.
	DuePendingReferences(ctx context.Context, limit int) ([]uuid.UUID, error)

	// Resume re-runs one of them: the repository rehydrates the record, locks
	// its wallet and resolves the reference, then applies the decision the
	// callback builds for that record. A record still PENDING_REFERENCE after
	// the decision is backed off for the next attempt.
	Resume(ctx context.Context, id uuid.UUID, decide func(*wagering.WagerTransaction) Decide) error
}

// Duplicated from walletapp so the two use case packages share no import (§4).
type IDGenerator interface {
	NewID() uuid.UUID
}

type UUIDv7 struct{}

func (UUIDv7) NewID() uuid.UUID { return uuid.NewV7() }

// ReferenceTTL bounds how long a reversal waits for its reference (§7). Named
// rather than a bare time.Duration so the Fx graph cannot confuse it with
// another one.
type ReferenceTTL time.Duration

type Service struct {
	repo         Repository
	ids          IDGenerator
	referenceTTL ReferenceTTL
	now          func() time.Time
}

func NewService(repo Repository, ids IDGenerator, referenceTTL ReferenceTTL) *Service {
	return &Service{
		repo:         repo,
		ids:          ids,
		referenceTTL: referenceTTL,
		now:          func() time.Time { return time.Now().UTC() },
	}
}

type SubmitParams struct {
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PlayerID                       uuid.UUID
	WalletID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           wagering.Kind
	Money                          money.Money
	ReferenceExternalTransactionID string

	// Inbox is set when the operation arrived on the queue, and zero when it
	// arrived over HTTP. It is transport metadata, so PayloadHash ignores it
	// (§9) and the two paths hash identically (§10).
	Inbox Inbox
}

// Inbox is the durable identity of an inbound message: §10 makes it the
// envelope's messageId, and §6.5 makes the record share the commit with the
// domain change it caused.
type Inbox struct {
	MessageID  string
	ReceivedAt time.Time
}

func (i Inbox) IsZero() bool { return i.MessageID == "" }

type Result struct {
	Transaction *wagering.WagerTransaction
	Replay      bool
}

func (s *Service) Submit(ctx context.Context, p SubmitParams) (Result, error) {
	now := s.now()
	hash := PayloadHash(p)

	t, err := wagering.NewExternal(wagering.NewExternalParams{
		ID:                             s.ids.NewID(),
		ProviderID:                     p.ProviderID,
		ExternalTransactionID:          p.ExternalTransactionID,
		IdempotencyKey:                 p.IdempotencyKey,
		PayloadHash:                    hash,
		WalletID:                       p.WalletID,
		PlayerID:                       p.PlayerID,
		RoundID:                        p.RoundID,
		GameID:                         p.GameID,
		Kind:                           p.Kind,
		Amount:                         p.Money,
		ReferenceExternalTransactionID: p.ReferenceExternalTransactionID,
		Now:                            now,
	})
	if err != nil {
		return Result{}, err
	}

	// No pre-read: the unique violation is the only duplicate check that also
	// holds against a submission racing this one in another process.
	err = s.repo.Process(ctx, t, p.Inbox, s.decide(t, now))
	switch {
	case err == nil:
		return Result{Transaction: t}, nil
	case errors.Is(err, ErrDuplicateMessage):
		return s.replayMessage(ctx, p, hash)
	case errors.Is(err, ErrDuplicate):
		return s.replay(ctx, p, hash)
	default:
		return Result{}, err
	}
}

// replayMessage answers a redelivery. §10 asks that the hash be verified: the
// same id carrying different content is a producer fault, not a repeat, and the
// consumer must not treat it as handled.
func (s *Service) replayMessage(ctx context.Context, p SubmitParams, hash string) (Result, error) {
	handled, err := s.repo.MessageHash(ctx, p.Inbox.MessageID)
	if err != nil {
		return Result{}, err
	}
	if handled != hash {
		return Result{}, ErrMessageConflict
	}
	return s.replay(ctx, p, hash)
}

// replay says what the unique violation meant. No record under this key means
// the other index fired: the operation already exists under a second key, which
// §9 forbids.
func (s *Service) replay(ctx context.Context, p SubmitParams, hash string) (Result, error) {
	stored, err := s.repo.ByIdempotencyKey(ctx, p.ProviderID, p.IdempotencyKey)
	switch {
	case errors.Is(err, ErrNotFound):
		return Result{}, ErrExternalIDConflict
	case err != nil:
		return Result{}, err
	case stored.PayloadHash() != hash:
		return Result{}, ErrPayloadConflict
	}
	return Result{Transaction: stored, Replay: true}, nil
}

// The balance reported here is the one a replay returns, even after later
// movements (§9).
func (s *Service) decide(t *wagering.WagerTransaction, now time.Time) Decide {
	return func(w *wallet.Wallet, ref *Reference) (*wallet.LedgerEntry, []events.Envelope, error) {
		// One code for "no such wallet" and "not this player's wallet": telling
		// them apart would enumerate wallets (§2). Neither has a balance to
		// report, so the rejection carries the zero Money (04 §7).
		if w == nil || w.PlayerID() != t.PlayerID() {
			return s.reject(t, wagering.FailureWalletNotFound, money.Money{}, now)
		}
		if w.Currency() != t.Amount().Currency() {
			return s.reject(t, wagering.FailureCurrencyMismatch, w.Balance(), now)
		}

		switch t.Kind() {
		case wagering.KindBet:
			entry, err := w.Debit(s.ids.NewID(), t.ID(), t.Amount(), now)
			return s.settle(t, w, entry, err, wagering.FailureInsufficientFunds, now)
		case wagering.KindWin:
			entry, err := w.Credit(s.ids.NewID(), t.ID(), t.Amount(), now)
			return s.settle(t, w, entry, err, wagering.FailureInsufficientFunds, now)
		case wagering.KindLoss:
			// §7: no movement — the money already left on the BET.
			return s.settle(t, w, nil, nil, "", now)
		case wagering.KindRefund, wagering.KindRollback:
			return s.reverse(t, w, ref, now)
		}
		return nil, nil, fmt.Errorf("%w: %s", ErrUnsupportedKind, t.Kind())
	}
}

// reverse applies §7's reversal rules against the resolved reference. Checks
// run before the movement, so a refused reversal leaves the wallet untouched.
func (s *Service) reverse(t *wagering.WagerTransaction, w *wallet.Wallet, ref *Reference, now time.Time) (*wallet.LedgerEntry, []events.Envelope, error) {
	// Absent, or present but not finished: both are references that are not
	// available yet, which §7 waits for rather than refuses (A.8.2).
	if ref == nil || !ref.Transaction.Status().IsTerminal() {
		// Already waiting, so this is the reference worker looking again. §7
		// requires the wait be bounded; on expiry it ends REJECTED with the
		// reference-not-found code, and otherwise the caller backs it off.
		if t.Status() == wagering.StatusPendingReference {
			if t.ReferenceExpired(now) {
				return s.reject(t, wagering.FailureReferenceNotFound, w.Balance(), now)
			}
			return nil, nil, nil
		}
		if ref != nil {
			if err := t.ResolveReference(ref.Transaction.ID()); err != nil {
				return nil, nil, err
			}
		}
		if err := t.MarkPendingReference(now.Add(time.Duration(s.referenceTTL)), now); err != nil {
			return nil, nil, err
		}
		return nil, s.outbox(t, nil, 0, now), nil
	}

	r := ref.Transaction
	if err := t.ResolveReference(r.ID()); err != nil {
		return nil, nil, err
	}

	direction, reversible := reversalDirection(t.Kind(), r.Kind())
	switch {
	case r.Status() != wagering.StatusProcessed:
		return s.reject(t, wagering.FailureReferenceNotProcessed, w.Balance(), now)
	case !reversible || !agrees(t, r):
		return s.reject(t, wagering.FailureReferenceMismatch, w.Balance(), now)
	case ref.Reversed:
		return s.reject(t, wagering.FailureReferenceAlreadyReversed, w.Balance(), now)
	}
	// By value, never by the text received: "25" and "25.00" are one amount
	// (A.3.1). The currencies already agree, so Cmp cannot error.
	if cmp, _ := t.Amount().Cmp(r.Amount()); cmp != 0 {
		return s.reject(t, wagering.FailureReferenceAmount, w.Balance(), now)
	}

	var (
		entry *wallet.LedgerEntry
		err   error
	)
	if direction == wallet.DirectionDebit {
		entry, err = w.Debit(s.ids.NewID(), t.ID(), t.Amount(), now)
	} else {
		entry, err = w.Credit(s.ids.NewID(), t.ID(), t.Amount(), now)
	}
	return s.settle(t, w, entry, err, wagering.FailureReversalExceedsBalance, now)
}

// §7's reversal table. A REFUND returns a BET; a ROLLBACK undoes a processed
// BET, WIN or REFUND with the opposite movement. Anything else — a REFUND of a
// WIN, a ROLLBACK of a LOSS or of a ROLLBACK — has no entry in that table.
func reversalDirection(kind, referenced wagering.Kind) (wallet.Direction, bool) {
	switch {
	case referenced == wagering.KindBet:
		return wallet.DirectionCredit, kind == wagering.KindRefund || kind == wagering.KindRollback
	case referenced == wagering.KindWin || referenced == wagering.KindRefund:
		return wallet.DirectionDebit, kind == wagering.KindRollback
	}
	return "", false
}

// §7: the operation and its reference must agree on provider, player, wallet,
// currency and round. Provider is absent because the lookup is keyed by it.
func agrees(t, r *wagering.WagerTransaction) bool {
	return t.PlayerID() == r.PlayerID() &&
		t.WalletID() == r.WalletID() &&
		t.RoundID() == r.RoundID() &&
		t.Amount().Currency() == r.Amount().Currency()
}

// settle turns the movement's outcome into the transaction's own. overdrawn is
// the code for a refused debit, which §7 requires differ between a bet and a
// reversal.
func (s *Service) settle(t *wagering.WagerTransaction, w *wallet.Wallet, entry *wallet.LedgerEntry, err error, overdrawn wagering.FailureCode, now time.Time) (*wallet.LedgerEntry, []events.Envelope, error) {
	switch {
	case errors.Is(err, wallet.ErrInsufficientFunds):
		return s.reject(t, overdrawn, w.Balance(), now)
	case err != nil:
		return nil, nil, err
	}
	if err := t.MarkProcessed(w.Balance(), now); err != nil {
		return nil, nil, err
	}
	return entry, s.outbox(t, entry, w.Version(), now), nil
}

func (s *Service) reject(t *wagering.WagerTransaction, code wagering.FailureCode, balance money.Money, now time.Time) (*wallet.LedgerEntry, []events.Envelope, error) {
	if err := t.Reject(code, balance, now); err != nil {
		return nil, nil, err
	}
	return nil, s.outbox(t, nil, 0, now), nil
}

// The events an outcome owes (§11). entry is nil when no money moved, which is
// what makes a LOSS produce WagerTransactionProcessed and no
// WalletBalanceChanged (§7).
//
// The transaction's id stands in as correlationId while nothing carries one;
// 16 replaces it with the request's or the message's.
func (s *Service) outbox(t *wagering.WagerTransaction, entry *wallet.LedgerEntry, walletVersion int64, now time.Time) []events.Envelope {
	switch t.Status() {
	case wagering.StatusRejected:
		return []events.Envelope{events.NewWagerTransactionRejected(s.ids.NewID(), t.ID(), t, now)}
	case wagering.StatusPendingReference:
		return []events.Envelope{events.NewWagerTransactionPendingReference(s.ids.NewID(), t.ID(), t, now)}
	}

	outbox := []events.Envelope{events.NewWagerTransactionProcessed(s.ids.NewID(), t.ID(), t, now)}
	if entry != nil {
		outbox = append(outbox, events.NewWalletBalanceChanged(s.ids.NewID(), t.ID(), entry, walletVersion, now))
	}
	return outbox
}

func (s *Service) ByID(ctx context.Context, id uuid.UUID) (*wagering.WagerTransaction, error) {
	return s.repo.ByID(ctx, id)
}

func (s *Service) ByExternalID(ctx context.Context, providerID, externalTransactionID string) (*wagering.WagerTransaction, error) {
	return s.repo.ByExternalID(ctx, providerID, externalTransactionID)
}
