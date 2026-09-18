//go:build integration

package main

import (
	"net/http"
	"testing"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"
	"uuid"
)

// the reversal overtakes the operation it undoes. It waits, and
// the worker applies it once the bet lands — under 12's full rules.
func TestAReversalDeliveredBeforeItsReferenceResolvesLater(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	early := api.bet(bet{
		externalID: "refund-1", key: "provider-a:refund-1",
		playerID: playerID, walletID: walletID, amount: "25.00",
		kind: "REFUND", reference: "bet-1",
	})
	if early.Status != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (%+v)", early.Status, http.StatusAccepted, early)
	}
	if balance := api.balance(walletID); balance != "100.00" {
		t.Fatalf("balance = %s, want nothing moved while waiting", balance)
	}

	placed := api.bet(bet{
		externalID: "bet-1", key: "provider-a:bet-1",
		playerID: playerID, walletID: walletID, amount: "25.00",
	})
	if placed.Status != http.StatusOK {
		t.Fatalf("the reference failed: %+v", placed)
	}

	api.awaitStatus(early.TransactionID, "PROCESSED")

	if balance := api.balance(walletID); balance != "100.00" {
		t.Errorf("balance = %s, want the debit placed and returned", balance)
	}
	if got := api.debits(walletID); got != 1 {
		t.Errorf("ledger debits = %d, want the bet's alone", got)
	}
	// the return is an entry of its own, on top of the opening and the bet.
	if got := api.ledger(walletID); len(got) != 3 {
		t.Errorf("ledger = %v, want three entries", got)
	}
}

// the wait is bounded by REFERENCE_TTL; on expiry the reversal is REJECTED
// with the reference-not-found code and the rejection event it owes.
func TestAWaitExpiresIntoAReferenceNotFoundRejection(t *testing.T) {
	t.Setenv("REFERENCE_TTL", "1s")
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	waiting := api.bet(bet{
		externalID: "refund-1", key: "provider-a:refund-1",
		playerID: playerID, walletID: walletID, amount: "25.00",
		kind: "REFUND", reference: "a-bet-never-sent",
	})
	if waiting.Status != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (%+v)", waiting.Status, http.StatusAccepted, waiting)
	}

	if code := api.awaitStatus(waiting.TransactionID, "REJECTED"); code != "REFERENCE_NOT_FOUND" {
		t.Errorf("failureCode = %q, want REFERENCE_NOT_FOUND", code)
	}
	if balance := api.balance(walletID); balance != "100.00" {
		t.Errorf("balance = %s, want nothing moved", balance)
	}

	types := api.outboxTypes(walletID)
	if len(types) == 0 || types[len(types)-1] != "WagerTransactionRejected" {
		t.Errorf("outbox = %v, want it to end in WagerTransactionRejected", types)
	}
}

// the wait lives in PostgreSQL, not in the process, so another
// instance picks it up after a restart.
func TestAWaitSurvivesARestartAndIsResumedByAnotherInstance(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	waiting := api.bet(bet{
		externalID: "refund-1", key: "provider-a:refund-1",
		playerID: playerID, walletID: walletID, amount: "25.00",
		kind: "REFUND", reference: "bet-1",
	})
	if waiting.Status != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (%+v)", waiting.Status, http.StatusAccepted, waiting)
	}

	api.stop()

	// A second instance, with its own connections and memory, against the same
	// database.
	var server *http.Server
	second := fxtest.New(t, options(), fx.Populate(&server))
	second.RequireStart()
	t.Cleanup(second.RequireStop)
	next := &wagering{
		t: t, base: "http://" + server.Addr, databaseURL: api.databaseURL,
		internal: api.internal, provider: api.provider,
	}

	placed := next.bet(bet{
		externalID: "bet-1", key: "provider-a:bet-1",
		playerID: playerID, walletID: walletID, amount: "25.00",
	})
	if placed.Status != http.StatusOK {
		t.Fatalf("the reference failed: %+v", placed)
	}

	next.awaitStatus(waiting.TransactionID, "PROCESSED")
	if balance := next.balance(walletID); balance != "100.00" {
		t.Errorf("balance = %s, want the debit returned by the second instance", balance)
	}
}
