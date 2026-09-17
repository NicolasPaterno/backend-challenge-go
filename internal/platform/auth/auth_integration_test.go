//go:build integration

package auth_test

import (
	"context"
	"testing"
	"time"

	"go.uber.org/fx/fxtest"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/auth"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
)

func verifier(t *testing.T) (*auth.Verifier, string) {
	t.Helper()

	issuer := testsupport.KeycloakIssuer(t)
	lifecycle := fxtest.NewLifecycle(t)
	v := auth.New(lifecycle, config.Config{
		StartupTimeout:   30 * time.Second,
		OIDCIssuerURL:    issuer,
		OIDCDiscoveryURL: issuer,
		OIDCAudience:     testsupport.APIAudience,
	})
	lifecycle.RequireStart()
	t.Cleanup(lifecycle.RequireStop)

	return v, issuer
}

// The provider identity comes from the token's claims and from nowhere the
// caller controls (§2).
func TestVerifyMapsClaimsToIdentity(t *testing.T) {
	v, issuer := verifier(t)

	tests := map[string]struct {
		client     string
		providerID string
		wallets    bool
	}{
		"internal service": {testsupport.InternalClient, "", true},
		"provider a":       {testsupport.ProviderAClient, "provider-a", false},
		"provider b":       {testsupport.ProviderBClient, "provider-b", false},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			identity, err := v.Verify(context.Background(), testsupport.Token(t, issuer, tc.client))
			if err != nil {
				t.Fatalf("Verify() error = %v, want nil", err)
			}
			if identity.ProviderID != tc.providerID {
				t.Errorf("ProviderID = %q, want %q", identity.ProviderID, tc.providerID)
			}
			if identity.HasScope(auth.ScopeWallets) != tc.wallets {
				t.Errorf("HasScope(%q) = %v, want %v (scopes %v)",
					auth.ScopeWallets, !tc.wallets, tc.wallets, identity.Scopes)
			}
			if identity.Subject == "" {
				t.Error("Subject is empty, want the service account's subject")
			}
		})
	}
}

func TestVerifyRefusesTokensThisAPIMustNotAccept(t *testing.T) {
	v, issuer := verifier(t)

	tests := map[string]func() string{
		"not a jwt":      func() string { return "not-a-token" },
		"unsigned alg":   func() string { return "eyJhbGciOiJub25lIn0.eyJpc3MiOiJ4In0." },
		"wrong audience": func() string { return testsupport.Token(t, issuer, testsupport.OutsiderClient) },
		"expired": func() string {
			token := testsupport.Token(t, issuer, testsupport.ExpiringClient)
			time.Sleep(2 * time.Second) // the client issues a one-second token
			return token
		},
	}

	for name, token := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), token()); err == nil {
				t.Fatal("Verify() error = nil, want the token refused")
			}
		})
	}
}

// A token is never trusted before the issuer is known.
func TestVerifyRefusesEverythingBeforeDiscovery(t *testing.T) {
	var unstarted auth.Verifier
	if _, err := unstarted.Verify(context.Background(), "anything"); err == nil {
		t.Fatal("Verify() error = nil, want the token refused")
	}
}
