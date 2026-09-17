package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func postTransaction(t *testing.T, key, body string) (*httptest.ResponseRecorder, Problem) {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/wagering/transactions", strings.NewReader(body))
	if key != "" {
		request.Header.Set(idempotencyKeyHeader, key)
	}

	recorder := httptest.NewRecorder()
	NewWageringHandler(nil, nil).submit(recorder, request)

	var problem Problem
	if err := json.NewDecoder(recorder.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	return recorder, problem
}

const validBet = `{
	"providerId":"","externalTransactionId":"transaction-123",
	"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1",
	"walletId":"0192f291-27dd-7d3f-8071-5f8685deef37",
	"roundId":"round-987","gameId":"fortune-chimp","kind":"BET",
	"money":{"amount":"25.00","currency":"BRL"}}`

// The handler is reached with no identity in the context, so providerId is ""
// in every case here: the token check is covered against Keycloak in cmd/api.
func TestSubmitReportsEveryInvalidFieldAtOnce(t *testing.T) {
	recorder, problem := postTransaction(t, "", validBet)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	fields := make([]string, 0, len(problem.Errors))
	for _, violation := range problem.Errors {
		fields = append(fields, violation.Field)
	}
	for _, want := range []string{"providerId", idempotencyKeyHeader} {
		if !slices.Contains(fields, want) {
			t.Errorf("violations = %v, want one on %q", fields, want)
		}
	}
}

func TestSubmitRefusesAMalformedBody(t *testing.T) {
	recorder, problem := postTransaction(t, "provider-a:transaction-123", "{")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if problem.Code != CodeMalformedBody {
		t.Errorf("code = %q, want %q", problem.Code, CodeMalformedBody)
	}
}
