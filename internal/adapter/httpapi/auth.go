package httpapi

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/auth"
)

type Guard struct {
	verifier *auth.Verifier
	logger   *slog.Logger
}

func NewGuard(verifier *auth.Verifier, logger *slog.Logger) *Guard {
	return &Guard{verifier: verifier, logger: logger}
}

// Require refuses before the handler runs, so a rejected request has no
// financial effect and its body carries no wallet data. An empty scope
// asks only for a valid token.
func (g *Guard) Require(scope string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rawToken, ok := bearerToken(r.Header.Get("Authorization"))
		if !ok {
			g.unauthenticated(w, "a bearer access token is required")
			return
		}

		identity, err := g.verifier.Verify(r.Context(), rawToken)
		if err != nil {
			// Why it failed is a log line: telling the caller distinguishes an
			// expired token from an unknown signer, which is theirs to find out.
			g.logger.WarnContext(r.Context(), "rejected access token", slog.Any("error", err))
			g.unauthenticated(w, "the access token is not valid")
			return
		}

		if scope != "" && !identity.HasScope(scope) {
			writeProblem(w, http.StatusForbidden, CodeForbidden,
				"the access token does not carry the "+scope+" scope")
			return
		}

		next.ServeHTTP(w, r.WithContext(auth.NewContext(r.Context(), identity)))
	})
}

func (g *Guard) unauthenticated(w http.ResponseWriter, detail string) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	writeProblem(w, http.StatusUnauthorized, CodeUnauthenticated, detail)
}

func bearerToken(header string) (string, bool) {
	scheme, token, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}
