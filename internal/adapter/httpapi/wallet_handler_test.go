package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

// The nil service is the assertion: every case here must be refused before
// the use case is reached.
func newValidationOnlyHandler() *WalletHandler { return NewWalletHandler(nil, nil) }

func postWallet(t *testing.T, body string) (*httptest.ResponseRecorder, Problem) {
	t.Helper()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/wallets", strings.NewReader(body))
	newValidationOnlyHandler().open(recorder, request)

	var problem Problem
	if err := json.NewDecoder(recorder.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	return recorder, problem
}

func TestOpenWalletReportsEveryInvalidFieldAtOnce(t *testing.T) {
	recorder, problem := postWallet(t, `{"playerId":"not-a-uuid","initialBalance":{"amount":"25.000","currency":"BRL"}}`)

	if recorder.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if got := recorder.Header().Get("Content-Type"); got != problemContentType {
		t.Errorf("Content-Type = %q, want %q", got, problemContentType)
	}
	if problem.Code != CodeValidationFailed {
		t.Errorf("code = %q, want %q", problem.Code, CodeValidationFailed)
	}

	fields := make([]string, 0, len(problem.Errors))
	for _, violation := range problem.Errors {
		fields = append(fields, violation.Field)
	}
	if !slices.Equal(fields, []string{"playerId", "initialBalance"}) {
		t.Errorf("violations = %v, want both playerId and initialBalance", problem.Errors)
	}
}

func TestOpenWalletValidation(t *testing.T) {
	tests := []struct {
		name  string
		body  string
		code  string
		field string
	}{
		{"missing player", `{"initialBalance":{"amount":"1.00","currency":"BRL"}}`, CodeValidationFailed, "playerId"},
		{"missing balance", `{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"}`, CodeValidationFailed, "initialBalance"},
		{"null balance", `{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":null}`, CodeValidationFailed, "initialBalance"},
		{"unknown currency", `{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1.00","currency":"ZZZ"}}`, CodeValidationFailed, "initialBalance"},
		{"negative amount", `{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"-1.00","currency":"BRL"}}`, CodeValidationFailed, "initialBalance"},
		{"malformed body", `{`, CodeMalformedBody, ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder, problem := postWallet(t, test.body)

			if recorder.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			if problem.Code != test.code {
				t.Errorf("code = %q, want %q", problem.Code, test.code)
			}
			if test.field == "" {
				return
			}
			if len(problem.Errors) != 1 || problem.Errors[0].Field != test.field {
				t.Errorf("violations = %v, want one on %q", problem.Errors, test.field)
			}
		})
	}
}

func TestGetWalletRejectsANonUUIDPath(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/wallets/nope", nil)
	request.SetPathValue("walletId", "nope")

	newValidationOnlyHandler().get(recorder, request)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
}
