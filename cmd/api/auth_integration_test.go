//go:build integration

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
)

func TestWalletRoutesAcceptOnlyTheInternalService(t *testing.T) {
	t.Setenv("DATABASE_URL", testsupport.PostgresMigrated(t))
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("LOG_LEVEL", "error")
	issuer := testsupport.KeycloakEnv(t)
	testsupport.SQSEnv(t)

	var server *http.Server
	fxApp := fxtest.New(t, options(), fx.Populate(&server))
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	// Unknown wallet: an authorized caller gets 404, so anything else is the
	// guard answering before the use case ran.
	endpoint := "http://" + server.Addr + "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37"

	tests := map[string]struct {
		header func() string
		want   int
	}{
		"no credentials": {func() string { return "" }, http.StatusUnauthorized},
		"not a token":    {func() string { return "Bearer not-a-token" }, http.StatusUnauthorized},
		"wrong scheme":   {func() string { return "Basic aW50ZXJuYWw6c2VjcmV0" }, http.StatusUnauthorized},
		"wrong audience": {func() string { return "Bearer " + testsupport.Token(t, issuer, testsupport.OutsiderClient) }, http.StatusUnauthorized},
		"provider token": {func() string { return "Bearer " + testsupport.Token(t, issuer, testsupport.ProviderAClient) }, http.StatusForbidden},
		"internal token": {func() string { return "Bearer " + testsupport.Token(t, issuer, testsupport.InternalClient) }, http.StatusNotFound},
		"expired token": {func() string {
			token := testsupport.Token(t, issuer, testsupport.ExpiringClient)
			time.Sleep(2 * time.Second) // the client issues a one-second token
			return "Bearer " + token
		}, http.StatusUnauthorized},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			request, err := http.NewRequest(http.MethodGet, endpoint, nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			if header := tc.header(); header != "" {
				request.Header.Set("Authorization", header)
			}

			resp, err := http.DefaultClient.Do(request)
			if err != nil {
				t.Fatalf("GET %s: %v", endpoint, err)
			}
			defer resp.Body.Close()

			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if got := resp.Header.Get("Content-Type"); got != "application/problem+json" {
				t.Errorf("Content-Type = %q, want application/problem+json", got)
			}
			if resp.StatusCode == http.StatusUnauthorized && resp.Header.Get("WWW-Authenticate") == "" {
				t.Error("401 carried no WWW-Authenticate header")
			}
		})
	}
}

// §13: a refused request must not leak what it was refused access to.
func TestUnauthorizedReadExposesNoWalletData(t *testing.T) {
	t.Setenv("DATABASE_URL", testsupport.PostgresMigrated(t))
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("LOG_LEVEL", "error")
	issuer := testsupport.KeycloakEnv(t)
	testsupport.SQSEnv(t)

	var server *http.Server
	fxApp := fxtest.New(t, options(), fx.Populate(&server))
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	base := "http://" + server.Addr
	const playerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"

	created, err := testsupport.BearerClient(t, issuer, testsupport.InternalClient).Post(base+"/wallets", "application/json",
		strings.NewReader(`{"playerId":"`+playerID+`","initialBalance":{"amount":"1000.00","currency":"BRL"}}`))
	if err != nil {
		t.Fatalf("POST /wallets: %v", err)
	}
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("POST /wallets status = %d, want %d", created.StatusCode, http.StatusCreated)
	}

	var wallet struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(created.Body).Decode(&wallet); err != nil {
		t.Fatalf("decode POST /wallets response: %v", err)
	}

	for _, path := range []string{"/wallets/" + wallet.ID, "/wallets/" + wallet.ID + "/ledger"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("GET %s status = %d, want %d", path, resp.StatusCode, http.StatusUnauthorized)
		}
		for _, leak := range []string{playerID, "1000.00", "BRL", "balance"} {
			if strings.Contains(string(body), leak) {
				t.Errorf("GET %s body leaked %q: %s", path, leak, body)
			}
		}
	}
}
