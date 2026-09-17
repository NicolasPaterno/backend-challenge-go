//go:build integration

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"uuid"

	"github.com/jackc/pgx/v5"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
)

type wagering struct {
	t           *testing.T
	base        string
	databaseURL string
	internal    *http.Client
	provider    *http.Client

	app     *fxtest.App
	stopped bool
}

func startWagering(t *testing.T) *wagering {
	t.Helper()

	databaseURL := testsupport.PostgresMigrated(t)
	t.Setenv("DATABASE_URL", databaseURL)
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	// Quiet unless a test wants the lines themselves, as the log-scrubbing one
	// does.
	if os.Getenv("LOG_LEVEL") == "" {
		t.Setenv("LOG_LEVEL", "error")
	}
	// The reference worker's cadence, so a wait resolves inside a test rather
	// than on the one-second production tick.
	t.Setenv("REFERENCE_POLL_INTERVAL", "200ms")
	issuer := testsupport.KeycloakEnv(t)
	testsupport.SQSEnv(t)

	var server *http.Server
	app := fxtest.New(t, options(), fx.Populate(&server))
	app.RequireStart()

	w := &wagering{
		t:           t,
		base:        "http://" + server.Addr,
		databaseURL: databaseURL,
		internal:    testsupport.BearerClient(t, issuer, testsupport.InternalClient),
		provider:    testsupport.BearerClient(t, issuer, testsupport.ProviderAClient),
		app:         app,
	}
	t.Cleanup(w.stop)
	return w
}

// stop is idempotent so a test may shut the instance down mid-way and still
// leave the cleanup in place.
func (w *wagering) stop() {
	if w.stopped {
		return
	}
	w.stopped = true
	w.app.RequireStop()
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

// kind, currency and reference are empty in the many BET cases below, where
// the zero value stands for the §9 example request.
type bet struct {
	externalID string
	key        string
	playerID   string
	walletID   string
	amount     string
	kind       string
	currency   string
	round      string
	reference  string
	client     *http.Client
}

func (w *wagering) bet(b bet) betResult {
	w.t.Helper()

	if b.kind == "" {
		b.kind = "BET"
	}
	if b.currency == "" {
		b.currency = "BRL"
	}
	if b.round == "" {
		b.round = "round-987"
	}

	request := fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,"playerId":%q,`+
		`"walletId":%q,"roundId":%q,"gameId":"fortune-chimp","kind":%q,`+
		`"money":{"amount":%q,"currency":%q},"referenceExternalTransactionId":%q}`,
		b.externalID, b.playerID, b.walletID, b.round, b.kind, b.amount, b.currency, b.reference)

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

// transaction reads one operation back, which is how a test observes work the
// reference worker finished after the request had already answered 202 (§9).
func (w *wagering) transaction(id string) (status, code string) {
	w.t.Helper()

	resp, err := w.provider.Get(w.base + "/wagering/transactions/" + id)
	if err != nil {
		w.t.Fatalf("GET /wagering/transactions/%s: %v", id, err)
	}
	defer resp.Body.Close()

	var read struct {
		Status      string `json:"status"`
		FailureCode string `json:"failureCode"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil {
		w.t.Fatalf("decode transaction: %v", err)
	}
	return read.Status, read.FailureCode
}

// awaitStatus polls until the worker has moved the record, or gives up. The
// worker is asynchronous by definition, so there is nothing to synchronise on
// from out here.
func (w *wagering) awaitStatus(id, want string) string {
	w.t.Helper()

	deadline := time.Now().Add(20 * time.Second)
	for {
		status, code := w.transaction(id)
		if status == want {
			return code
		}
		if time.Now().After(deadline) {
			w.t.Fatalf("transaction %s is %s after 20s, want %s", id, status, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (w *wagering) balance(walletID string) string {
	balance, _ := w.wallet(walletID)
	return balance
}

func (w *wagering) wallet(walletID string) (balance string, version int64) {
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
		Version int64 `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil {
		w.t.Fatalf("decode wallet: %v", err)
	}
	return read.Balance.Amount, read.Version
}

func (w *wagering) ledger(walletID string) []string {
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

	directions := make([]string, 0, len(page.Entries))
	for _, entry := range page.Entries {
		directions = append(directions, entry.Direction)
	}
	return directions
}

func (w *wagering) debits(walletID string) int {
	w.t.Helper()

	debits := 0
	for _, direction := range w.ledger(walletID) {
		if direction == "DEBIT" {
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

// §8: a wallet held past DB_LOCK_TIMEOUT answers 503 rather than queueing until
// the caller gives up.
func TestAContendedWalletIsRefusedNotQueued(t *testing.T) {
	t.Setenv("DB_LOCK_TIMEOUT", "300ms")
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, api.databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	held, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer held.Rollback(ctx)
	if _, err := held.Exec(ctx, "SELECT 1 FROM wallets WHERE id = $1 FOR UPDATE", walletID); err != nil {
		t.Fatalf("hold the wallet: %v", err)
	}

	refused := api.bet(bet{
		externalID: "transaction-1", key: "provider-a:transaction-1",
		playerID: playerID, walletID: walletID, amount: "25.00",
	})
	if refused.Status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (%+v)", refused.Status, http.StatusServiceUnavailable, refused)
	}

	if err := held.Rollback(ctx); err != nil {
		t.Fatalf("release the wallet: %v", err)
	}
	applied := api.bet(bet{
		externalID: "transaction-1", key: "provider-a:transaction-1",
		playerID: playerID, walletID: walletID, amount: "25.00",
	})
	if applied.Status != http.StatusOK || applied.IdempotentReplay {
		t.Fatalf("the retry did not apply the operation: %+v", applied)
	}
	if got := api.balance(walletID); got != "75.00" {
		t.Errorf("balance = %s, want 75.00", got)
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

func TestWinCreditsTheWallet(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	won := api.bet(bet{
		externalID: "transaction-win", key: "provider-a:transaction-win",
		playerID: playerID, walletID: walletID, amount: "25.00",
		kind: "WIN", reference: "transaction-never-submitted",
	})
	if won.Status != http.StatusOK {
		t.Fatalf("status = %d, want %d (%+v)", won.Status, http.StatusOK, won)
	}
	if won.Balance != "125.00" {
		t.Errorf("balance = %s, want 125.00", won.Balance)
	}

	balance, version := api.wallet(walletID)
	if balance != "125.00" {
		t.Errorf("stored balance = %s, want 125.00", balance)
	}
	if version != 2 {
		t.Errorf("version = %d, want 2", version)
	}
	if got := api.ledger(walletID); len(got) != 2 {
		t.Errorf("ledger entries = %v, want the opening credit and the WIN's", got)
	}

	replay := api.bet(bet{
		externalID: "transaction-win", key: "provider-a:transaction-win",
		playerID: playerID, walletID: walletID, amount: "25.00",
		kind: "WIN", reference: "transaction-never-submitted",
	})
	if !replay.IdempotentReplay || replay.Balance != "125.00" {
		t.Errorf("the resubmission did not replay the stored result: %+v", replay)
	}
	if balance, _ := api.wallet(walletID); balance != "125.00" {
		t.Errorf("balance after the replay = %s, want 125.00", balance)
	}
}

func TestLossSettlesWithoutMovingMoney(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")
	_, opened := api.wallet(walletID)

	lost := api.bet(bet{
		externalID: "transaction-loss", key: "provider-a:transaction-loss",
		playerID: playerID, walletID: walletID, amount: "0.00", kind: "LOSS",
	})
	if lost.Status != http.StatusOK {
		t.Fatalf("status = %d, want %d (%+v)", lost.Status, http.StatusOK, lost)
	}
	if lost.Balance != "100.00" {
		t.Errorf("balance = %s, want the unchanged 100.00", lost.Balance)
	}

	balance, version := api.wallet(walletID)
	if balance != "100.00" {
		t.Errorf("stored balance = %s, want 100.00", balance)
	}
	if version != opened {
		t.Errorf("version = %d, want the unchanged %d", version, opened)
	}
	if got := api.ledger(walletID); len(got) != 1 {
		t.Errorf("ledger entries = %v, want only the opening credit", got)
	}
}

// §7 writes the LOSS rule as money.amount == "0.00"; A.3.1 reads it as the
// value zero, so an equivalent spelling is accepted and a non-zero is refused.
func TestLossAmountPolicy(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	spelled := api.bet(bet{
		externalID: "transaction-zero", key: "provider-a:transaction-zero",
		playerID: playerID, walletID: walletID, amount: "0", kind: "LOSS",
	})
	if spelled.Status != http.StatusOK {
		t.Errorf(`a LOSS of "0" status = %d, want %d (%+v)`, spelled.Status, http.StatusOK, spelled)
	}

	// Refused by the constructor, so it is invalid input with no record and no
	// failure code (A.3.5), not a business rejection.
	nonZero := api.bet(bet{
		externalID: "transaction-nonzero", key: "provider-a:transaction-nonzero",
		playerID: playerID, walletID: walletID, amount: "25.00", kind: "LOSS",
	})
	if nonZero.Status != http.StatusBadRequest {
		t.Errorf("a non-zero LOSS status = %d, want %d (%+v)", nonZero.Status, http.StatusBadRequest, nonZero)
	}

	// Unlike the two above, decided against a real wallet: a recorded rejection,
	// not invalid input.
	foreign := api.bet(bet{
		externalID: "transaction-eur", key: "provider-a:transaction-eur",
		playerID: playerID, walletID: walletID, amount: "0.00", kind: "LOSS", currency: "EUR",
	})
	if foreign.Status != http.StatusUnprocessableEntity {
		t.Fatalf("a LOSS in EUR status = %d, want %d (%+v)", foreign.Status, http.StatusUnprocessableEntity, foreign)
	}
	if foreign.Code != "WALLET_CURRENCY_MISMATCH" {
		t.Errorf("failure code = %q, want WALLET_CURRENCY_MISMATCH", foreign.Code)
	}

	if balance, _ := api.wallet(walletID); balance != "100.00" {
		t.Errorf("balance = %s, want 100.00; no LOSS moves money", balance)
	}
}

// event_id is a UUIDv7 taken from the same generator in write order, so it is
// also the order the events were produced in.
const selectOutbox = `
	SELECT event_type, payload, published_at
	FROM outbox_events WHERE aggregate_id = $1 ORDER BY event_id`

type outboxRow struct {
	eventType string
	payload   map[string]any
	published *time.Time
}

func (w *wagering) connect() *pgx.Conn {
	w.t.Helper()

	conn, err := pgx.Connect(context.Background(), w.databaseURL)
	if err != nil {
		w.t.Fatalf("connect: %v", err)
	}
	return conn
}

func (w *wagering) outbox(walletID string) []outboxRow {
	w.t.Helper()

	ctx := context.Background()
	conn := w.connect()
	defer conn.Close(ctx)

	id, err := uuid.Parse(walletID)
	if err != nil {
		w.t.Fatalf("parse wallet id: %v", err)
	}
	rows, err := conn.Query(ctx, selectOutbox, id)
	if err != nil {
		w.t.Fatalf("select outbox: %v", err)
	}
	defer rows.Close()

	var out []outboxRow
	for rows.Next() {
		var (
			row outboxRow
			raw []byte
		)
		if err := rows.Scan(&row.eventType, &raw, &row.published); err != nil {
			w.t.Fatalf("scan outbox row: %v", err)
		}
		if err := json.Unmarshal(raw, &row.payload); err != nil {
			w.t.Fatalf("decode payload: %v", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		w.t.Fatalf("read outbox: %v", err)
	}
	return out
}

func (w *wagering) outboxTypes(walletID string) []string {
	w.t.Helper()

	var types []string
	for _, row := range w.outbox(walletID) {
		types = append(types, row.eventType)
	}
	return types
}

// §11: every outcome writes its events in the commit that caused it, and §5.4
// leaves them unpublished until 11 exists.
func TestOutcomesWriteTheirEventsToTheOutbox(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	if got := api.outboxTypes(walletID); !slices.Equal(got, []string{"WagerTransactionProcessed", "WalletBalanceChanged"}) {
		t.Fatalf("after opening, outbox = %v", got)
	}

	api.bet(bet{externalID: "t-bet", key: "provider-a:t-bet", playerID: playerID, walletID: walletID, amount: "25.00"})
	api.bet(bet{externalID: "t-loss", key: "provider-a:t-loss", playerID: playerID, walletID: walletID, amount: "0.00", kind: "LOSS"})
	api.bet(bet{externalID: "t-broke", key: "provider-a:t-broke", playerID: playerID, walletID: walletID, amount: "9999.00"})

	want := []string{
		"WagerTransactionProcessed", "WalletBalanceChanged", // the opening
		"WagerTransactionProcessed", "WalletBalanceChanged", // the BET
		"WagerTransactionProcessed", // the LOSS moves nothing (§7)
		"WagerTransactionRejected",  // insufficient funds
	}
	rows := api.outbox(walletID)
	if got := api.outboxTypes(walletID); !slices.Equal(got, want) {
		t.Fatalf("outbox = %v, want %v", got, want)
	}

	for _, row := range rows {
		if row.published != nil {
			t.Errorf("%s is published, but nothing publishes before 11 (§5.4)", row.eventType)
		}
		for _, field := range []string{"eventId", "eventType", "aggregateId", "correlationId", "occurredAt", "version", "data"} {
			if _, ok := row.payload[field]; !ok {
				t.Errorf("%s payload has no %q", row.eventType, field)
			}
		}
		if row.payload["eventType"] != row.eventType {
			t.Errorf("column says %s, payload says %v", row.eventType, row.payload["eventType"])
		}
	}

	// A replay re-applies nothing, so it owes no second copy (§9).
	api.bet(bet{externalID: "t-bet", key: "provider-a:t-bet", playerID: playerID, walletID: walletID, amount: "25.00"})
	if got := api.outboxTypes(walletID); !slices.Equal(got, want) {
		t.Errorf("after the replay, outbox = %v, want the unchanged %v", got, want)
	}
}

// §9: a zero opening creates no OPENING, no ledger entry and none of these
// financial events.
func TestZeroOpeningWritesNoEvents(t *testing.T) {
	api := startWagering(t)
	walletID := api.openWallet(uuid.NewV7().String(), "0.00")

	if got := api.outbox(walletID); len(got) != 0 {
		t.Errorf("outbox = %v, want none", got)
	}
}
