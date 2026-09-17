// Package httpapi is the HTTP edge: decoding, status mapping and the one error
// body the whole API uses. It holds no business rule.
package httpapi

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
)

// Problem is the single error body of the API (RFC 9457, A.3.5). Later stories
// reuse this shape and none defines a second one.
//
// Code draws on two vocabularies: a rejection decided once a transaction exists
// carries a wagering.FailureCode, while what a constructor refuses never becomes
// a record and carries one of the constants below instead.
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
	// Instance identifies this occurrence: for a business rejection, the
	// transaction the refusal was recorded against, so the provider can query
	// it (RFC 9457 §3.1.4, §9).
	Instance string `json:"instance,omitempty"`
	// §9 requires an equivalent resubmission to report the flag with the
	// persisted result, and a rejection's result is this body. Absent on a
	// problem that is not a submission outcome.
	IdempotentReplay *bool       `json:"idempotentReplay,omitempty"`
	Errors           []Violation `json:"errors,omitempty"`
}

// Every violation of a request is reported at once, so a caller fixes the
// request in one round trip (A.3.5).
type Violation struct {
	Field  string `json:"field"`
	Code   string `json:"code"`
	Detail string `json:"detail"`
}

const (
	CodeValidationFailed    = "VALIDATION_FAILED"
	CodeMalformedBody       = "MALFORMED_BODY"
	CodeWalletAlreadyExists = "WALLET_ALREADY_EXISTS"
	CodeWalletNotFound      = "WALLET_NOT_FOUND"
	CodeUnauthenticated     = "UNAUTHENTICATED"
	CodeForbidden           = "FORBIDDEN"
	CodeInternalError       = "INTERNAL_ERROR"
	CodeUnavailable         = "SERVICE_UNAVAILABLE"

	CodeTransactionNotFound         = "TRANSACTION_NOT_FOUND"
	CodeIdempotencyKeyConflict      = "IDEMPOTENCY_KEY_CONFLICT"
	CodeExternalTransactionConflict = "EXTERNAL_TRANSACTION_CONFLICT"

	ViolationRequired = "REQUIRED"
	ViolationInvalid  = "INVALID"
)

const problemContentType = "application/problem+json"

func writeProblem(w http.ResponseWriter, status int, code, detail string, violations ...Violation) {
	write(w, problemContentType, status, Problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Code:   code,
		Detail: detail,
		Errors: violations,
	})
}

// A rejection decided once the transaction exists carries its FailureCode as
// the problem code, which is the second of the two vocabularies (A.3.5).
func writeRejection(w http.ResponseWriter, status int, t *wagering.WagerTransaction, replay bool) {
	write(w, problemContentType, status, Problem{
		Type:             "about:blank",
		Title:            http.StatusText(status),
		Status:           status,
		Code:             t.FailureCode().String(),
		Detail:           "the operation was not applied: " + t.Status().String(),
		Instance:         "/wagering/transactions/" + t.ID().String(),
		IdempotentReplay: &replay,
	})
}

// §9 requires transient unavailability to be distinguishable from a permanent
// failure by the contract alone. pgx marks a failure that never reached the
// server as safe to retry; the interface is matched rather than imported, so
// the HTTP edge stays free of the driver.
func writeUnavailableOrInternal(w http.ResponseWriter, err error) {
	var retryable interface{ SafeToRetry() bool }
	if errors.As(err, &retryable) && retryable.SafeToRetry() {
		w.Header().Set("Retry-After", "1")
		writeProblem(w, http.StatusServiceUnavailable, CodeUnavailable,
			"the service could not reach a dependency; the request was not applied")
		return
	}
	writeProblem(w, http.StatusInternalServerError, CodeInternalError, "the request could not be completed")
}

// The cause is logged, never returned: §12 forbids leaking internals to callers.
func failInternal(logger *slog.Logger, w http.ResponseWriter, r *http.Request, operation string, err error) {
	logger.ErrorContext(r.Context(), operation+" failed", slog.Any("error", err))
	writeUnavailableOrInternal(w, err)
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	write(w, "application/json", status, body)
}

func write(w http.ResponseWriter, contentType string, status int, body any) {
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write response body", slog.Any("error", err))
	}
}
