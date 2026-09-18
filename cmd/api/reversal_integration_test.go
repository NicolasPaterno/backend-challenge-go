//go:build integration

package main

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"uuid"
)

// reversals walks the two reversal kinds over a wallet that opens at 100.00
// and bets 25.00, so every case below starts from a 75.00 balance.
type reversals struct {
	*wagering
	playerID string
	walletID string
}

func startReversals(t *testing.T) *reversals {
	t.Helper()

	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	r := &reversals{wagering: api, playerID: playerID, walletID: walletID}
	if placed := r.submit(bet{externalID: "bet-1", amount: "25.00"}); placed.Status != http.StatusOK {
		t.Fatalf("the opening bet failed: %+v", placed)
	}
	return r
}

func (r *reversals) submit(b bet) betResult {
	r.t.Helper()

	b.playerID, b.walletID = r.playerID, r.walletID
	if b.key == "" {
		b.key = "provider-a:" + b.externalID
	}
	return r.bet(b)
}

func TestRefundReturnsTheBetAndAppendsToTheLedger(t *testing.T) {
	api := startReversals(t)

	refunded := api.submit(bet{externalID: "refund-1", amount: "25.00", kind: "REFUND", reference: "bet-1"})
	if refunded.Status != http.StatusOK {
		t.Fatalf("status = %d, want %d (%+v)", refunded.Status, http.StatusOK, refunded)
	}
	if refunded.Balance != "100.00" {
		t.Errorf("balance = %s, want 100.00", refunded.Balance)
	}

	// the debit is never edited away; the return is its own entry.
	if got := api.ledger(api.walletID); len(got) != 3 {
		t.Errorf("ledger = %v, want the opening, the debit and the refund's credit", got)
	}
	if got := api.debits(api.walletID); got != 1 {
		t.Errorf("debits = %d, want the bet's alone", got)
	}
}

// a ROLLBACK applies the movement opposite to the one it undoes.
func TestRollbackUndoesEachOfItsTargets(t *testing.T) {
	tests := map[string]struct {
		target bet
		want   string
	}{
		"a bet": {bet{externalID: "bet-1", amount: "25.00"}, "100.00"},
		"a win": {bet{externalID: "win-1", amount: "40.00", kind: "WIN"}, "75.00"},
		"a refund": {bet{externalID: "refund-1", amount: "25.00", kind: "REFUND",
			reference: "bet-1"}, "75.00"},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			api := startReversals(t)
			if test.target.externalID != "bet-1" {
				if placed := api.submit(test.target); placed.Status != http.StatusOK {
					t.Fatalf("the target failed: %+v", placed)
				}
			}

			undone := api.submit(bet{
				externalID: "rollback-1", amount: test.target.amount,
				kind: "ROLLBACK", reference: test.target.externalID,
			})
			if undone.Status != http.StatusOK {
				t.Fatalf("status = %d, want %d (%+v)", undone.Status, http.StatusOK, undone)
			}
			if undone.Balance != test.want {
				t.Errorf("balance = %s, want %s", undone.Balance, test.want)
			}
			if balance, _ := api.wallet(api.walletID); balance != test.want {
				t.Errorf("stored balance = %s, want %s", balance, test.want)
			}
		})
	}
}

// A.8.1: one debit, one return. The second reversal is refused whichever kind
// it carries — a per-kind rule would let the ROLLBACK through and pay twice.
func TestASecondReversalOfOneBetIsRefused(t *testing.T) {
	for _, kind := range []string{"REFUND", "ROLLBACK"} {
		t.Run(kind, func(t *testing.T) {
			api := startReversals(t)
			if first := api.submit(bet{externalID: "refund-1", amount: "25.00",
				kind: "REFUND", reference: "bet-1"}); first.Status != http.StatusOK {
				t.Fatalf("the first refund failed: %+v", first)
			}

			again := api.submit(bet{externalID: "second-1", amount: "25.00", kind: kind, reference: "bet-1"})
			if again.Status != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want %d (%+v)", again.Status, http.StatusUnprocessableEntity, again)
			}
			if again.Code != "REFERENCE_ALREADY_REVERSED" {
				t.Errorf("code = %q, want REFERENCE_ALREADY_REVERSED", again.Code)
			}
			if balance, _ := api.wallet(api.walletID); balance != "100.00" {
				t.Errorf("balance = %s, want the debit returned exactly once", balance)
			}
		})
	}
}

// The brief lists REFUND as a rollback target, which is how a refund is undone
// without a second reversal of the bet (A.8.1).
func TestARefundIsUndoneByRollingTheRefundBack(t *testing.T) {
	api := startReversals(t)
	api.submit(bet{externalID: "refund-1", amount: "25.00", kind: "REFUND", reference: "bet-1"})

	undone := api.submit(bet{externalID: "rollback-1", amount: "25.00", kind: "ROLLBACK", reference: "refund-1"})
	if undone.Status != http.StatusOK {
		t.Fatalf("status = %d, want %d (%+v)", undone.Status, http.StatusOK, undone)
	}
	if undone.Balance != "75.00" {
		t.Errorf("balance = %s, want the bet standing again at 75.00", undone.Balance)
	}
}

// the rule is in the schema too, not only in the use case. Promoting the
// refused reversal by hand is the shortest way to reach the index without the
// application's own check in front of it.
func TestTheSchemaRefusesASecondSuccessfulReversal(t *testing.T) {
	api := startReversals(t)
	api.submit(bet{externalID: "refund-1", amount: "25.00", kind: "REFUND", reference: "bet-1"})
	refused := api.submit(bet{externalID: "rollback-1", amount: "25.00", kind: "ROLLBACK", reference: "bet-1"})

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, api.databaseURL)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	_, err = conn.Exec(ctx,
		`UPDATE wager_transactions SET status = 'PROCESSED' WHERE id = $1`, refused.TransactionID)
	if err == nil {
		t.Fatal("a second successful reversal of one bet was stored; the unique index is missing")
	}
}

// the agreement and amount rules, and the codes that tell them apart.
func TestAReversalThatDoesNotMatchItsReferenceIsRejected(t *testing.T) {
	tests := map[string]struct {
		reversal bet
		want     string
	}{
		"another round": {bet{externalID: "r-round", amount: "25.00", kind: "REFUND",
			reference: "bet-1", round: "round-000"}, "REFERENCE_MISMATCH"},
		"another currency": {bet{externalID: "r-eur", amount: "25.00", kind: "REFUND",
			reference: "bet-1", currency: "EUR"}, "WALLET_CURRENCY_MISMATCH"},
		"a partial amount": {bet{externalID: "r-partial", amount: "10.00", kind: "REFUND",
			reference: "bet-1"}, "REFERENCE_AMOUNT_MISMATCH"},
		"a bet that does not exist": {bet{externalID: "r-missing", amount: "25.00", kind: "REFUND",
			reference: "bet-never-sent"}, ""},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			api := startReversals(t)
			result := api.submit(test.reversal)

			// A reference that has not arrived is waited on, not refused (A.8.2).
			want := http.StatusUnprocessableEntity
			if test.want == "" {
				want = http.StatusAccepted
			}
			if result.Status != want {
				t.Fatalf("status = %d, want %d (%+v)", result.Status, want, result)
			}
			if result.Code != test.want {
				t.Errorf("code = %q, want %q", result.Code, test.want)
			}
			if balance, _ := api.wallet(api.walletID); balance != "75.00" {
				t.Errorf("balance = %s, want the untouched 75.00", balance)
			}
		})
	}
}

// a reversal that cannot be covered gets a code of its own, so it is not
// filed as a routine bet without funds.
func TestAReversalThatOverdrawsIsRejectedWithItsOwnCode(t *testing.T) {
	api := startReversals(t)
	api.submit(bet{externalID: "win-1", amount: "40.00", kind: "WIN"})
	api.submit(bet{externalID: "bet-2", amount: "100.00"})

	undone := api.submit(bet{externalID: "rollback-1", amount: "40.00", kind: "ROLLBACK", reference: "win-1"})
	if undone.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d (%+v)", undone.Status, http.StatusUnprocessableEntity, undone)
	}
	if undone.Code != "REVERSAL_EXCEEDS_BALANCE" {
		t.Errorf("code = %q, want REVERSAL_EXCEEDS_BALANCE, which must stay distinct from INSUFFICIENT_FUNDS", undone.Code)
	}
	if balance, _ := api.wallet(api.walletID); balance != "15.00" {
		t.Errorf("balance = %s, want the untouched 15.00", balance)
	}
}

// A.8.2: a REJECTED or FAILED reference is terminal and can never become
// PROCESSED, so it is refused now rather than waited on, with a code distinct
// from the reference-not-found a wait eventually gives up with.
func TestAReversalOfARejectedReferenceIsRejectedAtOnce(t *testing.T) {
	api := startReversals(t)

	overdrawn := api.submit(bet{externalID: "bet-2", amount: "1000.00"})
	if overdrawn.Code != "INSUFFICIENT_FUNDS" {
		t.Fatalf("the reference ended %+v, want a rejected bet", overdrawn)
	}

	reversal := api.submit(bet{externalID: "refund-2", amount: "1000.00", kind: "REFUND", reference: "bet-2"})
	if reversal.Status != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d rather than a %d wait (%+v)",
			reversal.Status, http.StatusUnprocessableEntity, http.StatusAccepted, reversal)
	}
	if reversal.Code != "REFERENCE_NOT_PROCESSED" {
		t.Errorf("code = %q, want REFERENCE_NOT_PROCESSED", reversal.Code)
	}
	if balance, _ := api.wallet(api.walletID); balance != "75.00" {
		t.Errorf("balance = %s, want the untouched 75.00", balance)
	}
}

// A REFUND and a ROLLBACK racing for one bet. Exactly one returns the debit and
// the other is a business rejection naming its reason — never a 409, which is
// what the whole-table unique-violation mapping used to turn this into (the brief:
// these situations must be distinguishable from the contract alone).
func TestTwoReversalsRacingForOneBetReturnItOnce(t *testing.T) {
	api := startReversals(t)

	kinds := []string{"REFUND", "ROLLBACK"}
	results := make([]betResult, len(kinds))
	var wg sync.WaitGroup
	for i, kind := range kinds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = api.submit(bet{
				externalID: "reversal-" + kind, amount: "25.00", kind: kind, reference: "bet-1",
			})
		}()
	}
	wg.Wait()

	applied := 0
	for _, result := range results {
		switch result.Status {
		case http.StatusOK:
			applied++
		case http.StatusUnprocessableEntity:
			if result.Code != "REFERENCE_ALREADY_REVERSED" {
				t.Errorf("code = %q, want REFERENCE_ALREADY_REVERSED", result.Code)
			}
		default:
			t.Errorf("status = %d, want 200 or 422 (%+v)", result.Status, result)
		}
	}
	if applied != 1 {
		t.Errorf("%d reversals applied, want 1", applied)
	}
	if balance, _ := api.wallet(api.walletID); balance != "100.00" {
		t.Errorf("balance = %s, want the debit returned exactly once", balance)
	}
	if got := api.debits(api.walletID); got != 1 {
		t.Errorf("ledger debits = %d, want the bet's alone", got)
	}
}
