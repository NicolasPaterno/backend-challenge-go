//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
)

func TestReadinessFollowsItsDependenciesAndHealthStaysPublic(t *testing.T) {
	t.Setenv("DATABASE_URL", testsupport.PostgresMigrated(t))
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("LOG_LEVEL", "error")
	testsupport.KeycloakEnv(t)
	testsupport.SQSEnv(t)

	var (
		server *http.Server
		pool   *pgxpool.Pool
	)
	app := fxtest.New(t, options(), fx.Populate(&server, &pool))
	app.RequireStart()
	defer app.RequireStop()

	base := "http://" + server.Addr
	// No token anywhere in this test: the health checks are public.
	ready := func() (int, map[string]string) {
		resp, err := http.Get(base + "/health/ready")
		if err != nil {
			t.Fatalf("GET /health/ready: %v", err)
		}
		defer resp.Body.Close()

		var body struct {
			Status string            `json:"status"`
			Checks map[string]string `json:"checks"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
			t.Fatalf("decode readiness: %v", err)
		}
		return resp.StatusCode, body.Checks
	}

	status, checks := ready()
	if status != http.StatusOK || checks["postgres"] != "ok" || checks["sqs"] != "ok" {
		t.Fatalf("readiness = %d %v, want 200 with both ok", status, checks)
	}

	live, err := http.Get(base + "/health/live")
	if err != nil {
		t.Fatalf("GET /health/live: %v", err)
	}
	live.Body.Close()
	if live.StatusCode != http.StatusOK {
		t.Errorf("liveness = %d, want 200", live.StatusCode)
	}

	pool.Close()

	status, checks = ready()
	if status != http.StatusServiceUnavailable || checks["postgres"] != "unavailable" {
		t.Errorf("readiness with postgres down = %d %v, want 503 with postgres unavailable", status, checks)
	}
	// SQS is untouched, so the probe still tells the two dependencies apart.
	if checks["sqs"] != "ok" {
		t.Errorf("sqs check = %q, want ok", checks["sqs"])
	}
}

func TestMetricsCountTheOutcomesAndThePublicEndpointServesThem(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	api.bet(bet{externalID: "obs-1", key: "provider-a:obs-1", playerID: playerID, walletID: walletID, amount: "10.00"})
	api.bet(bet{externalID: "obs-1", key: "provider-a:obs-1", playerID: playerID, walletID: walletID, amount: "10.00"})
	api.bet(bet{externalID: "obs-2", key: "provider-a:obs-2", playerID: playerID, walletID: walletID, amount: "500.00"})
	api.reconcile(walletID)

	resp, err := http.Get(api.base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}

	var published map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&published); err != nil {
		t.Fatalf("decode metrics: %v", err)
	}

	for _, name := range []string{
		"wager_outcomes", "wager_duplicates", "wager_processing", "reference_retries",
		"sqs_dead_lettered", "wallet_concurrency_conflicts", "reconciliation_divergences",
		"outbox_lag_seconds",
	} {
		if _, ok := published[name]; !ok {
			t.Errorf("metric %q is missing", name)
		}
	}

	var outcomes map[string]int64
	if err := json.Unmarshal(published["wager_outcomes"], &outcomes); err != nil {
		t.Fatalf("decode wager_outcomes: %v", err)
	}
	if outcomes["PROCESSED"] < 1 || outcomes["REJECTED"] < 1 {
		t.Errorf("wager_outcomes = %v, want at least one PROCESSED and one REJECTED", outcomes)
	}

	var duplicates int64
	if err := json.Unmarshal(published["wager_duplicates"], &duplicates); err != nil {
		t.Fatalf("decode wager_duplicates: %v", err)
	}
	if duplicates < 1 {
		t.Errorf("wager_duplicates = %d, want at least the one replay", duplicates)
	}
}

func TestCorrelationIdReachesTheEventItCaused(t *testing.T) {
	api := startWagering(t)
	correlationID := uuid.NewV7().String()
	playerID := uuid.NewV7().String()

	body := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":"50.00","currency":"BRL"}}`, playerID)
	request, err := http.NewRequest(http.MethodPost, api.base+"/wallets", strings.NewReader(body))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Correlation-Id", correlationID)

	resp, err := api.internal.Do(request)
	if err != nil {
		t.Fatalf("POST /wallets: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /wallets status = %d, want 201", resp.StatusCode)
	}
	if got := resp.Header.Get("X-Correlation-Id"); got != correlationID {
		t.Errorf("echoed correlation id = %q, want %q", got, correlationID)
	}

	var opened struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&opened); err != nil {
		t.Fatalf("decode wallet: %v", err)
	}

	for _, row := range api.outbox(opened.ID) {
		if row.payload["correlationId"] != correlationID {
			t.Errorf("%s correlationId = %v, want %q", row.eventType, row.payload["correlationId"], correlationID)
		}
	}
}

// the logs must carry the identifiers and none of the secrets.
func TestLogsCarryTheIdentifiersAndNoCredentials(t *testing.T) {
	t.Setenv("LOG_LEVEL", "debug")

	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	stdout := os.Stdout
	os.Stdout = write

	captured := make(chan []byte, 1)
	go func() {
		var buf bytes.Buffer
		_, _ = io.Copy(&buf, read)
		captured <- buf.Bytes()
	}()

	api := startWagering(t)
	token := testsupport.Token(t, os.Getenv("OIDC_ISSUER_URL"), testsupport.ProviderAClient)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")
	api.bet(bet{externalID: "log-1", key: "provider-a:log-1", playerID: playerID, walletID: walletID, amount: "10.00"})

	// A rejected token, which is the line most likely to echo one back.
	rejected, err := http.NewRequest(http.MethodGet, api.base+"/wallets/"+walletID, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	rejected.Header.Set("Authorization", "Bearer "+token+"-tampered")
	resp, err := http.DefaultClient.Do(rejected)
	if err != nil {
		t.Fatalf("GET with a tampered token: %v", err)
	}
	resp.Body.Close()

	api.stop()
	os.Stdout = stdout
	_ = write.Close()
	logs := string(<-captured)

	for _, secret := range []string{token, testsupport.ClientSecret(testsupport.ProviderAClient)} {
		if strings.Contains(logs, secret) {
			t.Errorf("a credential reached the logs")
		}
	}
	// The amounts are the financial payload the brief keeps out of the logs.
	if strings.Contains(logs, `"amount"`) || strings.Contains(logs, "10.00") {
		t.Errorf("a financial payload reached the logs")
	}
	for _, identifier := range []string{"correlationId", "walletId", "transactionId", "providerId"} {
		if !strings.Contains(logs, identifier) {
			t.Errorf("logs carry no %s", identifier)
		}
	}
	if !strings.Contains(logs, `"level":`) || !strings.Contains(logs, `"msg":`) {
		t.Errorf("logs are not the JSON handler's output")
	}
}
