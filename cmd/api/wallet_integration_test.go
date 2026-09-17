//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
)

// "1000" goes in and the canonical "1000.00" comes back (A.3.1).
func TestOpenAndReadWalletOverHTTP(t *testing.T) {
	t.Setenv("DATABASE_URL", testsupport.PostgresMigrated(t))
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("LOG_LEVEL", "warn")
	issuer := testsupport.KeycloakEnv(t)

	var server *http.Server
	fxApp := fxtest.New(t, options(), fx.Populate(&server))
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	base := "http://" + server.Addr
	client := testsupport.BearerClient(t, issuer, testsupport.InternalClient)
	const playerID = "0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1"
	body := `{"playerId":"` + playerID + `","initialBalance":{"amount":"1000","currency":"BRL"}}`

	post := func() *http.Response {
		t.Helper()
		resp, err := client.Post(base+"/wallets", "application/json", bytes.NewReader([]byte(body)))
		if err != nil {
			t.Fatalf("POST /wallets: %v", err)
		}
		return resp
	}

	created := post()
	defer created.Body.Close()
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("POST /wallets status = %d, want %d", created.StatusCode, http.StatusCreated)
	}

	var opened struct {
		ID      string `json:"id"`
		Balance struct {
			Amount   string `json:"amount"`
			Currency string `json:"currency"`
		} `json:"balance"`
		Version int64 `json:"version"`
	}
	if err := json.NewDecoder(created.Body).Decode(&opened); err != nil {
		t.Fatalf("decode created wallet: %v", err)
	}
	if opened.Balance.Amount != "1000.00" || opened.Balance.Currency != "BRL" {
		t.Errorf("balance = %s %s, want 1000.00 BRL", opened.Balance.Amount, opened.Balance.Currency)
	}
	if opened.Version != 1 {
		t.Errorf("version = %d, want 1", opened.Version)
	}

	duplicate := post()
	defer duplicate.Body.Close()
	if duplicate.StatusCode != http.StatusConflict {
		t.Errorf("second POST /wallets status = %d, want %d", duplicate.StatusCode, http.StatusConflict)
	}
	if got := duplicate.Header.Get("Content-Type"); got != "application/problem+json" {
		t.Errorf("conflict Content-Type = %q, want application/problem+json", got)
	}

	read, err := client.Get(base + "/wallets/" + opened.ID)
	if err != nil {
		t.Fatalf("GET /wallets/%s: %v", opened.ID, err)
	}
	defer read.Body.Close()
	if read.StatusCode != http.StatusOK {
		t.Fatalf("GET /wallets status = %d, want %d", read.StatusCode, http.StatusOK)
	}

	missing, err := client.Get(base + "/wallets/0192f291-27dd-7d3f-8071-5f8685deef37")
	if err != nil {
		t.Fatalf("GET unknown wallet: %v", err)
	}
	defer missing.Body.Close()
	if missing.StatusCode != http.StatusNotFound {
		t.Errorf("GET unknown wallet status = %d, want %d", missing.StatusCode, http.StatusNotFound)
	}
}
