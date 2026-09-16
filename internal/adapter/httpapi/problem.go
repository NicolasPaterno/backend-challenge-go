// Package httpapi is the HTTP edge: decoding, status mapping and the one error
// body the whole API uses. It holds no business rule.
package httpapi

import (
	"encoding/json"
	"log/slog"
	"net/http"
)

// Problem is the single error body of the API (RFC 9457, A.3.5). Later stories
// reuse this shape and none defines a second one.
//
// Code draws on two vocabularies: a rejection decided once a transaction exists
// carries a wagering.FailureCode, while what a constructor refuses never becomes
// a record and carries one of the constants below instead.
type Problem struct {
	Type   string      `json:"type"`
	Title  string      `json:"title"`
	Status int         `json:"status"`
	Code   string      `json:"code"`
	Detail string      `json:"detail,omitempty"`
	Errors []Violation `json:"errors,omitempty"`
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
	CodeInternalError       = "INTERNAL_ERROR"

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
