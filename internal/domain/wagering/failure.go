package wagering

// FailureCode is the stable, documented reason a transaction ended REJECTED or
// FAILED. Codes are part of the external contract: they travel in the
// rejection event and in the HTTP problem body, so a value here is never
// renamed, only retired.
//
// Membership rule: a code belongs here only if Reject or Fail can reach it,
// which means the transaction already exists. Everything the constructors
// refuse — a non-zero LOSS, an OPENING over HTTP, a reversal with no reference
// — produces no record to carry a code, and is reported by the problem body
// of 05 instead (A.3.5).
type FailureCode string

// Correctable input — the provider can resubmit a fixed request under a new
// idempotency key and expect a different outcome.
const (
	// FailureWalletNotFound also covers a wallet that exists but belongs to
	// another player: one code for both denies an enumeration oracle.
	FailureWalletNotFound    FailureCode = "WALLET_NOT_FOUND"
	FailureCurrencyMismatch  FailureCode = "WALLET_CURRENCY_MISMATCH"
	FailureReferenceMismatch FailureCode = "REFERENCE_MISMATCH"
	FailureReferenceAmount   FailureCode = "REFERENCE_AMOUNT_MISMATCH"
)

// Definitive outcomes — the same request resubmitted reaches the same end.
const (
	// Distinct from FailureReversalExceedsBalance by the brief: a bet refused for
	// funds is routine, a reversal that cannot be undone is an incident.
	FailureInsufficientFunds      FailureCode = "INSUFFICIENT_FUNDS"
	FailureReversalExceedsBalance FailureCode = "REVERSAL_EXCEEDS_BALANCE"

	// The reference never arrived within the TTL of 13.
	FailureReferenceNotFound FailureCode = "REFERENCE_NOT_FOUND"
	// The reference exists but is PENDING, REJECTED or FAILED, so there is no
	// processed movement to reverse.
	FailureReferenceNotProcessed FailureCode = "REFERENCE_NOT_PROCESSED"
	// A reversal of this kind already succeeded against the reference.
	FailureReferenceAlreadyReversed FailureCode = "REFERENCE_ALREADY_REVERSED"
	// Permanent infrastructure failure recorded for audit (the brief FAILED).
	FailureInternalError FailureCode = "INTERNAL_ERROR"
)

var correctable = map[FailureCode]bool{
	FailureWalletNotFound:           true,
	FailureCurrencyMismatch:         true,
	FailureReferenceMismatch:        true,
	FailureReferenceAmount:          true,
	FailureInsufficientFunds:        false,
	FailureReversalExceedsBalance:   false,
	FailureReferenceNotFound:        false,
	FailureReferenceNotProcessed:    false,
	FailureReferenceAlreadyReversed: false,
	FailureInternalError:            false,
}

// Correctable reports whether a corrected resubmission could succeed.
func (c FailureCode) Correctable() bool { return correctable[c] }

func (c FailureCode) IsValid() bool {
	_, ok := correctable[c]
	return ok
}

func (c FailureCode) String() string { return string(c) }
