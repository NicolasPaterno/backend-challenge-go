//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/events"
	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
)

// The whole path §11 asks for: the opening commits its events, the worker
// drains them, and they arrive on the queue after — never before — that commit.
func TestOpeningEventsReachTheQueue(t *testing.T) {
	t.Setenv("DATABASE_URL", testsupport.PostgresMigrated(t))
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("LOG_LEVEL", "warn")
	t.Setenv("OUTBOX_POLL_INTERVAL", "200ms")
	issuer := testsupport.KeycloakEnv(t)
	client, queueURL := testsupport.SQSEnv(t)

	var server *http.Server
	fxApp := fxtest.New(t, options(), fx.Populate(&server))
	fxApp.RequireStart()
	defer fxApp.RequireStop()

	body := `{"playerId":"0192f28f-5dc0-7d58-bdb2-814ad6a0f4a1","initialBalance":{"amount":"1000.00","currency":"BRL"}}`
	resp, err := testsupport.BearerClient(t, issuer, testsupport.InternalClient).
		Post("http://"+server.Addr+"/wallets", "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatalf("POST /wallets: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("POST /wallets status = %d, want %d", resp.StatusCode, http.StatusCreated)
	}

	var opened struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&opened); err != nil {
		t.Fatalf("decode created wallet: %v", err)
	}

	messages := testsupport.ReceiveAll(t, client, queueURL, 2)
	if len(messages) != 2 {
		t.Fatalf("received %d events, want 2", len(messages))
	}

	seen := map[string]bool{}
	for _, m := range messages {
		var envelope events.Envelope
		if err := json.Unmarshal([]byte(*m.Body), &envelope); err != nil {
			t.Fatalf("decode envelope %q: %v", *m.Body, err)
		}
		if envelope.AggregateID.String() != opened.ID {
			t.Errorf("aggregateId = %s, want the wallet %s", envelope.AggregateID, opened.ID)
		}
		if envelope.EventID == uuid.Nil() {
			t.Error("eventId is empty")
		}
		seen[envelope.EventType] = true
	}

	for _, want := range []string{events.TypeWagerTransactionProcessed, events.TypeWalletBalanceChanged} {
		if !seen[want] {
			t.Errorf("%s never reached the queue, got %v", want, seen)
		}
	}
}
