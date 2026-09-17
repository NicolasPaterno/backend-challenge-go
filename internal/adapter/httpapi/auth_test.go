package httpapi

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/auth"
)

// The zero Verifier has discovered no issuer, so it trusts nothing: a token
// that reaches it is refused rather than accepted unverified.
func guard() *Guard {
	return NewGuard(&auth.Verifier{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestGuardRefusesUnusableCredentials(t *testing.T) {
	tests := map[string]string{
		"no header":        "",
		"bare token":       "an-access-token",
		"wrong scheme":     "Basic dXNlcjpwYXNz",
		"empty bearer":     "Bearer ",
		"unverifiable jwt": "Bearer eyJhbGciOiJub25lIn0.e30.",
	}

	for name, header := range tests {
		t.Run(name, func(t *testing.T) {
			reached := false
			handler := guard().Require(auth.ScopeWallets, func(http.ResponseWriter, *http.Request) {
				reached = true
			})

			request := httptest.NewRequest(http.MethodGet, "/wallets/x", nil)
			if header != "" {
				request.Header.Set("Authorization", header)
			}
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)

			if reached {
				t.Fatal("the handler ran; an unauthenticated request must have no effect")
			}
			if recorder.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want %d", recorder.Code, http.StatusUnauthorized)
			}
			if got := recorder.Header().Get("WWW-Authenticate"); got != "Bearer" {
				t.Errorf("WWW-Authenticate = %q, want %q", got, "Bearer")
			}
		})
	}
}
