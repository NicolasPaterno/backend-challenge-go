// Package wagering holds the operation record: one row per financial request,
// its state machine, and the rules each kind must satisfy.
package wagering

import (
	"errors"
	"fmt"
	"slices"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
)

var (
	ErrUninitialized      = errors.New("wagering: value is uninitialised")
	ErrInvalidKind        = errors.New("wagering: kind must be one of OPENING, BET, WIN, LOSS, REFUND, ROLLBACK")
	ErrInvalidStatus      = errors.New("wagering: status is not a known state")
	ErrInvalidOrigin      = errors.New("wagering: origin must be INTERNAL or EXTERNAL")
	ErrInvalidFailureCode = errors.New("wagering: failure code is not in the catalogue")
	ErrOpeningIsInternal  = errors.New("wagering: OPENING may not originate outside the service")
	ErrAmountNotZero      = errors.New("wagering: kind requires an amount equal to zero")
	ErrAmountNotPositive  = errors.New("wagering: kind requires an amount greater than zero")
	ErrMissingReference   = errors.New("wagering: kind requires referenceExternalTransactionId")
	ErrNoReference        = errors.New("wagering: no referenceExternalTransactionId applies to this operation")
	ErrInvalidTransition  = errors.New("wagering: transition is not permitted")
)

// IsRefusal reports an error a constructor raised because the request itself is
// malformed — a non-zero LOSS, an OPENING from outside, a reversal with no
// reference. None of them produces a record, so none carries a FailureCode
// (A.3.5), and no retry changes the answer: HTTP reports them as invalid input
// and the consumer treats them as permanent.
func IsRefusal(err error) bool {
	refusals := []error{
		ErrOpeningIsInternal,
		ErrInvalidKind,
		ErrUninitialized,
		ErrAmountNotZero,
		ErrAmountNotPositive,
		ErrMissingReference,
		ErrNoReference,
		money.ErrNegativeAmount,
	}
	return slices.ContainsFunc(refusals, func(refusal error) bool { return errors.Is(err, refusal) })
}

// TransitionError names both ends of a refused transition so a caller can log
// what it tried. errors.Is matches it against ErrInvalidTransition.
type TransitionError struct{ From, To Status }

func (e *TransitionError) Error() string {
	return fmt.Sprintf("wagering: transition %s -> %s is not permitted", e.From, e.To)
}

func (e *TransitionError) Is(target error) bool { return target == ErrInvalidTransition }

type Kind string

const (
	KindOpening  Kind = "OPENING"
	KindBet      Kind = "BET"
	KindWin      Kind = "WIN"
	KindLoss     Kind = "LOSS"
	KindRefund   Kind = "REFUND"
	KindRollback Kind = "ROLLBACK"
)

func (k Kind) IsValid() bool {
	switch k {
	case KindOpening, KindBet, KindWin, KindLoss, KindRefund, KindRollback:
		return true
	}
	return false
}

// IsReversal reports the kinds that undo an earlier operation and therefore
// require a reference.
func (k Kind) IsReversal() bool { return k == KindRefund || k == KindRollback }

func (k Kind) String() string { return string(k) }

type Status string

const (
	StatusPending          Status = "PENDING"
	StatusPendingReference Status = "PENDING_REFERENCE"
	StatusProcessed        Status = "PROCESSED"
	StatusRejected         Status = "REJECTED"
	StatusFailed           Status = "FAILED"
)

// Terminal states are absent as keys: a transition out of one is refused by the
// lookup itself, which is what makes a replay read the stored result instead of
// re-applying the operation.
var transitions = map[Status][]Status{
	StatusPending:          {StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed},
	StatusPendingReference: {StatusProcessed, StatusRejected, StatusFailed},
}

func (s Status) IsValid() bool {
	switch s {
	case StatusPending, StatusPendingReference, StatusProcessed, StatusRejected, StatusFailed:
		return true
	}
	return false
}

func (s Status) IsTerminal() bool {
	_, open := transitions[s]
	return s.IsValid() && !open
}

func (s Status) String() string { return string(s) }

type Origin string

const (
	OriginInternal Origin = "INTERNAL"
	OriginExternal Origin = "EXTERNAL"
)

func (o Origin) IsValid() bool { return o == OriginInternal || o == OriginExternal }

// WagerTransaction is one financial request and its outcome. External fields
// are empty for an INTERNAL origin and required for an EXTERNAL one (A.1).
type WagerTransaction struct {
	id       uuid.UUID
	origin   Origin
	kind     Kind
	status   Status
	walletID uuid.UUID
	playerID uuid.UUID
	amount   money.Money

	providerID            string
	externalTransactionID string
	idempotencyKey        string
	payloadHash           string
	roundID               string
	gameID                string

	referenceExternalTransactionID string
	referenceTransactionID         uuid.UUID
	referenceDeadlineAt            time.Time

	failureCode   FailureCode
	resultBalance money.Money

	createdAt time.Time
	updatedAt time.Time
}

// NewExternalParams carries the fields of an operation arriving over HTTP or
// SQS. A struct rather than thirteen positional arguments, six of them adjacent
// strings that would swap silently (the brief took the opposite call for the
// wallet's four).
type NewExternalParams struct {
	ID                             uuid.UUID
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	WalletID                       uuid.UUID
	PlayerID                       uuid.UUID
	RoundID                        string
	GameID                         string
	Kind                           Kind
	Amount                         money.Money
	ReferenceExternalTransactionID string
	Now                            time.Time
}

// NewExternal accepts an operation from a provider. It refuses OPENING, which
// is reserved for internal wallet opening (A.1 requirement 2), so both
// transports reject it through the one constructor.
func NewExternal(p NewExternalParams) (*WagerTransaction, error) {
	if p.Kind == KindOpening {
		return nil, ErrOpeningIsInternal
	}
	if err := checkCommon(p.ID, p.WalletID, p.PlayerID, p.Kind, p.Amount, p.Now); err != nil {
		return nil, err
	}
	required := []struct {
		name  string
		value string
	}{
		{"providerId", p.ProviderID},
		{"externalTransactionId", p.ExternalTransactionID},
		{"idempotencyKey", p.IdempotencyKey},
		{"payloadHash", p.PayloadHash},
		{"roundId", p.RoundID},
		{"gameId", p.GameID},
	}
	for _, field := range required {
		if field.value == "" {
			return nil, fmt.Errorf("%w: %s", ErrUninitialized, field.name)
		}
	}
	if err := checkReference(p.Kind, p.ReferenceExternalTransactionID); err != nil {
		return nil, err
	}

	return &WagerTransaction{
		id:                             p.ID,
		origin:                         OriginExternal,
		kind:                           p.Kind,
		status:                         StatusPending,
		walletID:                       p.WalletID,
		playerID:                       p.PlayerID,
		amount:                         p.Amount,
		providerID:                     p.ProviderID,
		externalTransactionID:          p.ExternalTransactionID,
		idempotencyKey:                 p.IdempotencyKey,
		payloadHash:                    p.PayloadHash,
		roundID:                        p.RoundID,
		gameID:                         p.GameID,
		referenceExternalTransactionID: p.ReferenceExternalTransactionID,
		createdAt:                      p.Now,
		updatedAt:                      p.Now,
	}, nil
}

// NewInternalOpeningParams has no provider, external ID, key, hash, round, game
// or reference field: none applies to an internally originated operation
// (A.1 requirement 1).
type NewInternalOpeningParams struct {
	ID       uuid.UUID
	WalletID uuid.UUID
	PlayerID uuid.UUID
	Amount   money.Money
	Now      time.Time
}

// NewInternalOpening records the credit that opens a wallet. The ID comes from
// the caller's IDGenerator and is never derived from the wallet (A.1).
func NewInternalOpening(p NewInternalOpeningParams) (*WagerTransaction, error) {
	if err := checkCommon(p.ID, p.WalletID, p.PlayerID, KindOpening, p.Amount, p.Now); err != nil {
		return nil, err
	}
	return &WagerTransaction{
		id:        p.ID,
		origin:    OriginInternal,
		kind:      KindOpening,
		status:    StatusPending,
		walletID:  p.WalletID,
		playerID:  p.PlayerID,
		amount:    p.Amount,
		createdAt: p.Now,
		updatedAt: p.Now,
	}, nil
}

// RehydrateParams is every persisted column. Rehydrate rebuilds the record from one
// without re-running kind rules or re-applying any movement.
type RehydrateParams struct {
	ID                             uuid.UUID
	Origin                         Origin
	Kind                           Kind
	Status                         Status
	WalletID                       uuid.UUID
	PlayerID                       uuid.UUID
	Amount                         money.Money
	ProviderID                     string
	ExternalTransactionID          string
	IdempotencyKey                 string
	PayloadHash                    string
	RoundID                        string
	GameID                         string
	ReferenceExternalTransactionID string
	ReferenceTransactionID         uuid.UUID
	ReferenceDeadlineAt            time.Time
	FailureCode                    FailureCode
	ResultBalance                  money.Money
	CreatedAt                      time.Time
	UpdatedAt                      time.Time
}

func Rehydrate(p RehydrateParams) (*WagerTransaction, error) {
	if p.ID == uuid.Nil() {
		return nil, fmt.Errorf("%w: id", ErrUninitialized)
	}
	if p.WalletID == uuid.Nil() {
		return nil, fmt.Errorf("%w: walletId", ErrUninitialized)
	}
	if p.PlayerID == uuid.Nil() {
		return nil, fmt.Errorf("%w: playerId", ErrUninitialized)
	}
	if !p.Origin.IsValid() {
		return nil, fmt.Errorf("%w, got %q", ErrInvalidOrigin, string(p.Origin))
	}
	if !p.Kind.IsValid() {
		return nil, fmt.Errorf("%w, got %q", ErrInvalidKind, string(p.Kind))
	}
	if (p.Origin == OriginInternal) != (p.Kind == KindOpening) {
		return nil, fmt.Errorf("%w: %s origin with kind %s", ErrInvalidOrigin, p.Origin, p.Kind)
	}
	if !p.Status.IsValid() {
		return nil, fmt.Errorf("%w, got %q", ErrInvalidStatus, string(p.Status))
	}
	if !p.Amount.IsValid() {
		return nil, fmt.Errorf("%w: money", ErrUninitialized)
	}
	if p.FailureCode != "" && !p.FailureCode.IsValid() {
		return nil, fmt.Errorf("%w, got %q", ErrInvalidFailureCode, string(p.FailureCode))
	}
	if p.CreatedAt.IsZero() {
		return nil, fmt.Errorf("%w: createdAt", ErrUninitialized)
	}
	if p.UpdatedAt.IsZero() {
		return nil, fmt.Errorf("%w: updatedAt", ErrUninitialized)
	}

	return &WagerTransaction{
		id:                             p.ID,
		origin:                         p.Origin,
		kind:                           p.Kind,
		status:                         p.Status,
		walletID:                       p.WalletID,
		playerID:                       p.PlayerID,
		amount:                         p.Amount,
		providerID:                     p.ProviderID,
		externalTransactionID:          p.ExternalTransactionID,
		idempotencyKey:                 p.IdempotencyKey,
		payloadHash:                    p.PayloadHash,
		roundID:                        p.RoundID,
		gameID:                         p.GameID,
		referenceExternalTransactionID: p.ReferenceExternalTransactionID,
		referenceTransactionID:         p.ReferenceTransactionID,
		referenceDeadlineAt:            p.ReferenceDeadlineAt,
		failureCode:                    p.FailureCode,
		resultBalance:                  p.ResultBalance,
		createdAt:                      p.CreatedAt,
		updatedAt:                      p.UpdatedAt,
	}, nil
}

func (t *WagerTransaction) ID() uuid.UUID { return t.id }

func (t *WagerTransaction) Origin() Origin { return t.origin }

func (t *WagerTransaction) Kind() Kind { return t.kind }

func (t *WagerTransaction) Status() Status { return t.status }

func (t *WagerTransaction) WalletID() uuid.UUID { return t.walletID }

func (t *WagerTransaction) PlayerID() uuid.UUID { return t.playerID }

func (t *WagerTransaction) Amount() money.Money { return t.amount }

func (t *WagerTransaction) ProviderID() string { return t.providerID }

func (t *WagerTransaction) ExternalTransactionID() string { return t.externalTransactionID }

func (t *WagerTransaction) IdempotencyKey() string { return t.idempotencyKey }

func (t *WagerTransaction) PayloadHash() string { return t.payloadHash }

func (t *WagerTransaction) RoundID() string { return t.roundID }

func (t *WagerTransaction) GameID() string { return t.gameID }

func (t *WagerTransaction) ReferenceExternalTransactionID() string {
	return t.referenceExternalTransactionID
}

func (t *WagerTransaction) ReferenceTransactionID() uuid.UUID { return t.referenceTransactionID }

// ReferenceDeadlineAt is when this wait stops being retried and becomes a
// rejection. Zero unless the record is, or has been, PENDING_REFERENCE.
func (t *WagerTransaction) ReferenceDeadlineAt() time.Time { return t.referenceDeadlineAt }

func (t *WagerTransaction) FailureCode() FailureCode { return t.failureCode }

// ResultBalance is the balance reported back when this transaction reached
// PROCESSED or REJECTED, which a replay returns unchanged even after later
// movements. Invalid when none was recorded.
func (t *WagerTransaction) ResultBalance() money.Money { return t.resultBalance }

func (t *WagerTransaction) CreatedAt() time.Time { return t.createdAt }

func (t *WagerTransaction) UpdatedAt() time.Time { return t.updatedAt }

func (t *WagerTransaction) MarkProcessed(resultBalance money.Money, now time.Time) error {
	if !resultBalance.IsValid() {
		return fmt.Errorf("%w: resultBalance", ErrUninitialized)
	}
	if resultBalance.IsNegative() {
		return fmt.Errorf("%w: resultBalance %s", money.ErrNegativeAmount, resultBalance)
	}
	if err := t.transition(StatusProcessed, now); err != nil {
		return err
	}
	t.resultBalance = resultBalance
	return nil
}

// ResolveReference records the internal id the lookup by
// (providerId, referenceExternalTransactionId) found, so a reversal keeps a
// link to what it undid even when it is then rejected.
func (t *WagerTransaction) ResolveReference(id uuid.UUID) error {
	if !t.kind.IsReversal() {
		return fmt.Errorf("%w: %s", ErrNoReference, t.kind)
	}
	if id == uuid.Nil() {
		return fmt.Errorf("%w: referenceTransactionId", ErrUninitialized)
	}
	t.referenceTransactionID = id
	return nil
}

// MarkPendingReference records the wait and the deadline it is accepted under.
// The deadline is stamped here, once, so a later change of REFERENCE_TTL cannot
// expire a reversal that was already waiting.
func (t *WagerTransaction) MarkPendingReference(deadlineAt, now time.Time) error {
	if deadlineAt.IsZero() {
		return fmt.Errorf("%w: referenceDeadlineAt", ErrUninitialized)
	}
	if !deadlineAt.After(now) {
		return fmt.Errorf("%w: referenceDeadlineAt %s is not after %s", ErrUninitialized, deadlineAt, now)
	}
	if err := t.transition(StatusPendingReference, now); err != nil {
		return err
	}
	t.referenceDeadlineAt = deadlineAt
	return nil
}

// ReferenceExpired reports that the wait has run out.
func (t *WagerTransaction) ReferenceExpired(now time.Time) bool {
	return t.status == StatusPendingReference && !now.Before(t.referenceDeadlineAt)
}

// Reject records a business refusal. The code is the caller's to choose: the
// wallet reports insufficient funds with one sentinel and cannot know whether
// it refused a bet or a reversal, which the brief requires be told apart.
//
// resultBalance is the balance to report back, which a replay must return
// unchanged. Pass the zero money.Money when there is none to report — a
// rejection for an unknown wallet has no balance to observe.
func (t *WagerTransaction) Reject(code FailureCode, resultBalance money.Money, now time.Time) error {
	if resultBalance.IsValid() && resultBalance.IsNegative() {
		return fmt.Errorf("%w: resultBalance %s", money.ErrNegativeAmount, resultBalance)
	}
	if err := t.finish(StatusRejected, code, now); err != nil {
		return err
	}
	t.resultBalance = resultBalance
	return nil
}

// Fail records a permanent infrastructure failure for audit. Transient
// failures are retried instead and never reach this method.
func (t *WagerTransaction) Fail(code FailureCode, now time.Time) error {
	return t.finish(StatusFailed, code, now)
}

func (t *WagerTransaction) finish(to Status, code FailureCode, now time.Time) error {
	if !code.IsValid() {
		return fmt.Errorf("%w, got %q", ErrInvalidFailureCode, string(code))
	}
	if err := t.transition(to, now); err != nil {
		return err
	}
	t.failureCode = code
	return nil
}

// The single writer of status: a rejection is an error return, never a panic.
func (t *WagerTransaction) transition(to Status, now time.Time) error {
	if now.IsZero() {
		return fmt.Errorf("%w: now", ErrUninitialized)
	}
	for _, permitted := range transitions[t.status] {
		if permitted == to {
			t.status = to
			t.updatedAt = now
			return nil
		}
	}
	return &TransitionError{From: t.status, To: to}
}

func checkCommon(id, walletID, playerID uuid.UUID, kind Kind, amount money.Money, now time.Time) error {
	if id == uuid.Nil() {
		return fmt.Errorf("%w: id", ErrUninitialized)
	}
	if walletID == uuid.Nil() {
		return fmt.Errorf("%w: walletId", ErrUninitialized)
	}
	if playerID == uuid.Nil() {
		return fmt.Errorf("%w: playerId", ErrUninitialized)
	}
	if !kind.IsValid() {
		return fmt.Errorf("%w, got %q", ErrInvalidKind, string(kind))
	}
	if !amount.IsValid() {
		return fmt.Errorf("%w: money", ErrUninitialized)
	}
	if now.IsZero() {
		return fmt.Errorf("%w: now", ErrUninitialized)
	}
	return checkAmount(kind, amount)
}

// the per-kind reference policy. A reversal must cite what it undoes; a WIN
// "may cite a bet from the same round", so a reference is optional there; a BET
// or a LOSS has nothing to reference and carrying one is a malformed request,
// not a field to ignore.
func checkReference(kind Kind, reference string) error {
	switch {
	case kind.IsReversal() && reference == "":
		return fmt.Errorf("%w: %s", ErrMissingReference, kind)
	case reference != "" && !kind.IsReversal() && kind != KindWin:
		return fmt.Errorf("%w: %s", ErrNoReference, kind)
	}
	return nil
}

// the per-kind amount policy. LOSS is tested with IsZero, not against the
// literal "0.00": "0" and "0.0" are the same amount after A.3.1's
// normalisation, and a string comparison would refuse a valid LOSS.
func checkAmount(kind Kind, amount money.Money) error {
	switch kind {
	case KindLoss:
		if !amount.IsZero() {
			return fmt.Errorf("%w: %s got %s", ErrAmountNotZero, kind, amount)
		}
	case KindOpening:
		// The brief accepts zero as an initial balance; the use case skips the
		// OPENING entirely in that case, so only a negative is refused.
		if amount.IsNegative() {
			return fmt.Errorf("%w: %s got %s", money.ErrNegativeAmount, kind, amount)
		}
	default:
		if amount.IsZero() || amount.IsNegative() {
			return fmt.Errorf("%w: %s got %s", ErrAmountNotPositive, kind, amount)
		}
	}
	return nil
}
