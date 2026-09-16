package httpapi

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
)

func encodeRaw(payload string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload))
}

func getLedger(t *testing.T, query string) (*httptest.ResponseRecorder, Problem) {
	t.Helper()

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/wallets/x/ledger?"+query, nil)
	request.SetPathValue("walletId", uuid.NewV7().String())
	newValidationOnlyHandler().ledger(recorder, request)

	var problem Problem
	if err := json.NewDecoder(recorder.Body).Decode(&problem); err != nil {
		t.Fatalf("decode problem body: %v", err)
	}
	return recorder, problem
}

func TestLedgerRejectsUnusableQueries(t *testing.T) {
	tests := []struct {
		name  string
		query string
		field string
	}{
		{"a cursor that is not base64", "cursor=not!base64", "cursor"},
		{"a cursor that is base64 of nothing useful", "cursor=YWJj", "cursor"},
		{"a cursor with an unparsable time", "cursor=" + encodeRaw("nope|"+uuid.NewV7().String()), "cursor"},
		{"a cursor with an unparsable id", "cursor=" + encodeRaw(time.Now().Format(time.RFC3339Nano)+"|nope"), "cursor"},
		{"a non-numeric limit", "limit=many", "limit"},
		{"a zero limit", "limit=0", "limit"},
		{"a negative limit", "limit=-1", "limit"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder, problem := getLedger(t, test.query)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			if len(problem.Errors) != 1 || problem.Errors[0].Field != test.field {
				t.Errorf("violations = %+v, want one on %q", problem.Errors, test.field)
			}
		})
	}
}

func TestCursorRoundTripsTheSortKey(t *testing.T) {
	amount, err := money.Parse("1.00", money.BRL)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	zero, err := money.Zero(money.BRL)
	if err != nil {
		t.Fatalf("Zero() error = %v", err)
	}

	at := time.Now().UTC().Truncate(time.Nanosecond)
	entry, err := wallet.NewLedgerEntry(uuid.NewV7(), uuid.NewV7(), uuid.NewV7(),
		wallet.DirectionCredit, amount, zero, amount, at)
	if err != nil {
		t.Fatalf("NewLedgerEntry() error = %v", err)
	}

	decoded, err := decodeCursor(encodeCursor(entry))
	if err != nil {
		t.Fatalf("decodeCursor() error = %v", err)
	}
	if !decoded.CreatedAt.Equal(at) || decoded.ID != entry.ID() {
		t.Errorf("cursor = %v/%s, want %v/%s", decoded.CreatedAt, decoded.ID, at, entry.ID())
	}
}
