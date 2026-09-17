// Package auth verifies OIDC access tokens against the issuer's JWKS (§2). A
// token is never decoded without verification.
package auth

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
)

// ScopeWallets guards the wallet surface, which §2 restricts to the internal
// service. Provider tokens do not carry it.
const ScopeWallets = "wallets"

// ScopeWagering guards the operation surface, which only providers reach. The
// scope says the caller may submit at all; which provider it may act as comes
// from the provider_id claim and from nowhere else (§2).
const ScopeWagering = "wagering"

var errNotDiscovered = errors.New("auth: the issuer has not been discovered yet")

// tokenTypeBearer is the value Keycloak puts in an access token's typ claim; an
// ID token carries "ID" and a refresh token "Refresh".
const tokenTypeBearer = "Bearer"

// Identity is what the token says the caller is. ProviderID comes from the
// token and from nowhere else, so a request body cannot claim another provider
// (§2).
type Identity struct {
	Subject    string
	ProviderID string
	Scopes     []string
}

func (i Identity) HasScope(scope string) bool { return slices.Contains(i.Scopes, scope) }

type contextKey struct{}

func NewContext(ctx context.Context, identity Identity) context.Context {
	return context.WithValue(ctx, contextKey{}, identity)
}

func FromContext(ctx context.Context) (Identity, bool) {
	identity, ok := ctx.Value(contextKey{}).(Identity)
	return identity, ok
}

type Verifier struct {
	verifier *oidc.IDTokenVerifier
}

// New discovers the issuer during startup rather than in the constructor, so an
// IdP that is still booting fails the start hook with a named dependency error
// instead of a graph that refuses to build (§4).
func New(lc fx.Lifecycle, cfg config.Config) *Verifier {
	v := &Verifier{}

	lc.Append(fx.Hook{OnStart: func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
		defer cancel()

		// The metadata may be served under a different host than the one the
		// tokens name; the issuer is still checked, against the configured value.
		ctx = oidc.InsecureIssuerURLContext(ctx, cfg.OIDCIssuerURL)

		provider, err := oidc.NewProvider(ctx, cfg.OIDCDiscoveryURL)
		if err != nil {
			return fmt.Errorf("discover oidc issuer %s: %w", cfg.OIDCDiscoveryURL, err)
		}
		// Pinned: left empty, go-oidc inherits every algorithm the metadata
		// advertises, which for Keycloak includes the HMAC family. The realm
		// signs with RS256 and nothing else may be accepted.
		v.verifier = provider.Verifier(&oidc.Config{
			ClientID:             cfg.OIDCAudience,
			SupportedSigningAlgs: []string{oidc.RS256},
		})
		return nil
	}})

	return v
}

// Any failure is one error: the caller must not learn which check refused it.
func (v *Verifier) Verify(ctx context.Context, rawToken string) (Identity, error) {
	if v.verifier == nil {
		return Identity{}, errNotDiscovered
	}

	token, err := v.verifier.Verify(ctx, rawToken)
	if err != nil {
		return Identity{}, fmt.Errorf("auth: verify token: %w", err)
	}

	var claims struct {
		Type       string `json:"typ"`
		ProviderID string `json:"provider_id"`
		Scope      string `json:"scope"`
	}
	if err := token.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("auth: read claims: %w", err)
	}

	// Keycloak marks the token's purpose in typ. Without this an ID token that
	// somehow carried the API's audience would authorize a call, which is the
	// token-confusion §2 asks the resource server to refuse.
	if claims.Type != tokenTypeBearer {
		return Identity{}, fmt.Errorf("auth: token type %q is not an access token", claims.Type)
	}

	return Identity{
		Subject:    token.Subject,
		ProviderID: claims.ProviderID,
		Scopes:     strings.Fields(claims.Scope),
	}, nil
}

var Module = fx.Module("auth", fx.Provide(New))
