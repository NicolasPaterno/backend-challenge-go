//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"uuid"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
)

type wagering struct {
	t        *testing.T
	base     string
	internal *http.Client
	provider *http.Client
}

func startWagering(t *testing.T) *wagering {
	t.Helper()

	t.Setenv("DATABASE_URL", testsupport.PostgresMigrated(t))
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("LOG_LEVEL", "error")
	issuer := testsupport.KeycloakEnv(t)

	var server *http.Server
	app := fxtest.New(t, options(), fx.Populate(&server))
	app.RequireStart()
	t.Cleanup(app.RequireStop)

	return &wagering{
		t:        t,
		base:     "http://" + server.Addr,
		internal: testsupport.BearerClient(t, issuer, testsupport.InternalClient),
		provider: testsupport.BearerClient(t, issuer, testsupport.ProviderAClient),
	}
}

func (w *wagering) openWallet(playerID, balance string) string {
	w.t.Helper()

	body := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":"BRL"}}`, playerID, balance)
	resp, err := w.internal.Post(w.base+"/wallets", "application/json", strings.NewReader(body))
	if err != nil {
		w.t.Fatalf("POST /wallets: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		w.t.Fatalf("POST /wallets status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}

	var opened struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&opened); err != nil {
		w.t.Fatalf("decode opened wallet: %v", err)
	}
	return opened.ID
}

type betResult struct {
	Status           int
	TransactionID    string
	IdempotentReplay bool
	Balance          string
	// Code is the problem body's failure code; empty on a success.
	Code string
}

type bet struct {
	externalID string
	key        string
	playerID   string
	walletID   string
	amount     string
	client     *http.Client
}

func (w *wagering) bet(b bet) betResult {
	w.t.Helper()

	request := fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,"playerId":%q,`+
		`"walletId":%q,"roundId":"round-987","gameId":"fortune-chimp","kind":"BET",`+
		`"money":{"amount":%q,"currency":"BRL"}}`, b.externalID, b.playerID, b.walletID, b.amount)

	post, err := http.NewRequest(http.MethodPost, w.base+"/wagering/transactions", strings.NewReader(request))
	if err != nil {
		w.t.Fatalf("build request: %v", err)
	}
	post.Header.Set("Content-Type", "application/json")
	post.Header.Set("Idempotency-Key", b.key)

	client := b.client
	if client == nil {
		client = w.provider
	}
	resp, err := client.Do(post)
	if err != nil {
		w.t.Fatalf("POST /wagering/transactions: %v", err)
	}
	defer resp.Body.Close()

	// The success body and the problem body disagree on what "status" means.
	var body struct {
		TransactionID    string `json:"transactionId"`
		IdempotentReplay bool   `json:"idempotentReplay"`
		Balance          struct {
			Amount string `json:"amount"`
		} `json:"balance"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		w.t.Fatalf("decode response (status %d): %v", resp.StatusCode, err)
	}
	return betResult{
		Status:           resp.StatusCode,
		TransactionID:    body.TransactionID,
		IdempotentReplay: body.IdempotentReplay,
		Balance:          body.Balance.Amount,
		Code:             body.Code,
	}
}

func (w *wagering) balance(walletID string) string {
	w.t.Helper()

	resp, err := w.internal.Get(w.base + "/wallets/" + walletID)
	if err != nil {
		w.t.Fatalf("GET /wallets/%s: %v", walletID, err)
	}
	defer resp.Body.Close()

	var read struct {
		Balance struct {
			Amount string `json:"amount"`
		} `json:"balance"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil {
		w.t.Fatalf("decode wallet: %v", err)
	}
	return read.Balance.Amount
}

func (w *wagering) debits(walletID string) int {
	w.t.Helper()

	resp, err := w.internal.Get(w.base + "/wallets/" + walletID + "/ledger?limit=200")
	if err != nil {
		w.t.Fatalf("GET ledger: %v", err)
	}
	defer resp.Body.Close()

	var page struct {
		Entries []struct {
			Direction string `json:"direction"`
		} `json:"entries"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		w.t.Fatalf("decode ledger: %v", err)
	}

	debits := 0
	for _, entry := range page.Entries {
		if entry.Direction == "DEBIT" {
			debits++
		}
	}
	return debits
}

// §13.1.
func TestSameBetInParallelDebitsOnce(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	const attempts = 50
	results := make([]betResult, attempts)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = api.bet(bet{
				externalID: "transaction-123", key: "provider-a:transaction-123",
				playerID: playerID, walletID: walletID, amount: "25.00",
			})
		}()
	}
	wg.Wait()

	applied := 0
	for _, result := range results {
		if result.Status != http.StatusOK {
			t.Fatalf("status = %d, want %d (%+v)", result.Status, http.StatusOK, result)
		}
		if !result.IdempotentReplay {
			applied++
		}
	}
	if applied != 1 {
		t.Errorf("%d submissions applied the operation, want 1", applied)
	}
	if got := api.balance(walletID); got != "75.00" {
		t.Errorf("balance = %s, want 75.00", got)
	}
	if got := api.debits(walletID); got != 1 {
		t.Errorf("ledger debits = %d, want 1", got)
	}
}

// §8, §13.2.
func TestTwoBetsRaceForOneBalance(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	bets := []bet{
		{externalID: "transaction-a", key: "provider-a:transaction-a", playerID: playerID, walletID: walletID, amount: "80.00"},
		{externalID: "transaction-b", key: "provider-a:transaction-b", playerID: playerID, walletID: walletID, amount: "80.00"},
	}

	results := make([]betResult, len(bets))
	var wg sync.WaitGroup
	for i, b := range bets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = api.bet(b)
		}()
	}
	wg.Wait()

	processed, rejected := 0, 0
	for _, result := range results {
		switch result.Status {
		case http.StatusOK:
			processed++
		case http.StatusUnprocessableEntity:
			rejected++
			if result.Code != "INSUFFICIENT_FUNDS" {
				t.Errorf("failure code = %q, want INSUFFICIENT_FUNDS", result.Code)
			}
		default:
			t.Fatalf("status = %d (%+v)", result.Status, result)
		}
	}
	if processed != 1 || rejected != 1 {
		t.Fatalf("processed = %d, rejected = %d, want 1 and 1", processed, rejected)
	}
	if got := api.balance(walletID); got != "20.00" {
		t.Errorf("balance = %s, want 20.00", got)
	}
	if got := api.debits(walletID); got != 1 {
		t.Errorf("ledger debits = %d, want 1", got)
	}

	// §9: an equivalent resubmission reports the flag with the persisted
	// result, rejection included.
	for _, b := range bets {
		replay := api.bet(b)
		if !replay.IdempotentReplay {
			t.Errorf("resubmission of %s (status %d) did not report idempotentReplay", b.externalID, replay.Status)
		}
	}
	if got := api.balance(walletID); got != "20.00" {
		t.Errorf("balance after resubmission = %s, want 20.00", got)
	}
}

// §13.3.
func TestDistinctWalletsProceedInParallel(t *testing.T) {
	api := startWagering(t)

	const wallets = 8
	players := make([]string, wallets)
	ids := make([]string, wallets)
	for i := range players {
		players[i] = uuid.NewV7().String()
		ids[i] = api.openWallet(players[i], "100.00")
	}

	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result := api.bet(bet{
				externalID: fmt.Sprintf("transaction-%d", i), key: fmt.Sprintf("provider-a:transaction-%d", i),
				playerID: players[i], walletID: ids[i], amount: "25.00",
			})
			if result.Status != http.StatusOK {
				t.Errorf("wallet %d: status = %d (%+v)", i, result.Status, result)
			}
		}()
	}
	wg.Wait()

	for i, id := range ids {
		if got := api.balance(id); got != "75.00" {
			t.Errorf("wallet %d balance = %s, want 75.00", i, got)
		}
	}
}

// §9.
func TestReplayReturnsTheOriginalBalance(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	first := bet{externalID: "transaction-1", key: "provider-a:transaction-1", playerID: playerID, walletID: walletID, amount: "25.00"}
	if result := api.bet(first); result.Balance != "75.00" {
		t.Fatalf("balance = %s, want 75.00 (%+v)", result.Balance, result)
	}

	second := bet{externalID: "transaction-2", key: "provider-a:transaction-2", playerID: playerID, walletID: walletID, amount: "10.00"}
	if result := api.bet(second); result.Balance != "65.00" {
		t.Fatalf("balance = %s, want 65.00", result.Balance)
	}

	// "25" is the same amount as "25.00" after A.3.1, so this is a replay.
	spelled := first
	spelled.amount = "25"
	replay := api.bet(spelled)
	if !replay.IdempotentReplay {
		t.Error("the resubmission did not report idempotentReplay")
	}
	if replay.Balance != "75.00" {
		t.Errorf("replayed balance = %s, want the 75.00 observed at processing", replay.Balance)
	}

	conflicting := first
	conflicting.amount = "30.00"
	if result := api.bet(conflicting); result.Status != http.StatusConflict {
		t.Errorf("conflicting payload status = %d, want %d", result.Status, http.StatusConflict)
	}

	rekeyed := first
	rekeyed.key = "provider-a:another-key"
	if result := api.bet(rekeyed); result.Status != http.StatusConflict {
		t.Errorf("second key status = %d, want %d", result.Status, http.StatusConflict)
	}
	if got := api.balance(walletID); got != "65.00" {
		t.Errorf("balance = %s, want 65.00; a conflict must move no money", got)
	}
}

// §11 owes a rejection event to an unknown wallet, so the refusal is a record,
// not a 404 that leaves nothing behind.
func TestBetAgainstAnUnknownWalletIsRecorded(t *testing.T) {
	api := startWagering(t)

	submitted := bet{
		externalID: "transaction-1", key: "provider-a:transaction-1",
		playerID: uuid.NewV7().String(), walletID: uuid.NewV7().String(), amount: "25.00",
	}
	rejected := api.bet(submitted)
	if rejected.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d (%+v)", rejected.Status, http.StatusUnprocessableEntity, rejected)
	}
	if rejected.Code != "WALLET_NOT_FOUND" {
		t.Errorf("failure code = %q, want WALLET_NOT_FOUND", rejected.Code)
	}

	resp, err := api.provider.Get(api.base + "/providers/provider-a/wagering/transactions/transaction-1")
	if err != nil {
		t.Fatalf("GET transaction: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the rejection was not persisted: status = %d", resp.StatusCode)
	}

	var stored struct {
		Status      string `json:"status"`
		FailureCode string `json:"failureCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&stored); err != nil {
		t.Fatalf("decode transaction: %v", err)
	}
	if stored.Status != "REJECTED" || stored.FailureCode != "WALLET_NOT_FOUND" {
		t.Errorf("stored = %s/%s, want REJECTED/WALLET_NOT_FOUND", stored.Status, stored.FailureCode)
	}

	if replay := api.bet(submitted); !replay.IdempotentReplay {
		t.Error("the resubmission did not report idempotentReplay")
	}
}

// §2, §13.
func TestProvidersAreIsolated(t *testing.T) {
	api := startWagering(t)
	issuer := testsupport.KeycloakIssuer(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	mine := api.bet(bet{
		externalID: "transaction-1", key: "provider-a:transaction-1",
		playerID: playerID, walletID: walletID, amount: "25.00",
	})
	if mine.Status != http.StatusOK {
		t.Fatalf("status = %d (%+v)", mine.Status, mine)
	}

	providerB := testsupport.BearerClient(t, issuer, testsupport.ProviderBClient)

	tests := map[string]struct {
		client *http.Client
		path   string
		want   int
	}{
		"own transaction by id":       {api.provider, "/wagering/transactions/" + mine.TransactionID, http.StatusOK},
		"own transaction by external": {api.provider, "/providers/provider-a/wagering/transactions/transaction-1", http.StatusOK},
		// Not 403: that would confirm the id exists.
		"another provider by id":       {providerB, "/wagering/transactions/" + mine.TransactionID, http.StatusNotFound},
		"another provider by external": {providerB, "/providers/provider-a/wagering/transactions/transaction-1", http.StatusForbidden},
		"internal token has no scope":  {api.internal, "/wagering/transactions/" + mine.TransactionID, http.StatusForbidden},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			resp, err := test.client.Get(api.base + test.path)
			if err != nil {
				t.Fatalf("GET %s: %v", test.path, err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != test.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, test.want)
			}
		})
	}

	// A submission naming another provider is refused by the token, not the body.
	crossed := api.bet(bet{
		externalID: "transaction-2", key: "provider-a:transaction-2",
		playerID: playerID, walletID: walletID, amount: "25.00", client: providerB,
	})
	if crossed.Status != http.StatusForbidden {
		t.Errorf("cross-provider submission status = %d, want %d", crossed.Status, http.StatusForbidden)
	}
	if got := api.balance(walletID); got != "75.00" {
		t.Errorf("balance = %s, want 75.00; a refused request must move no money", got)
	}
}
