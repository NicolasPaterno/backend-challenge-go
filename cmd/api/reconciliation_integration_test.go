//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/metrics"
)

type reconciliation struct {
	WalletID          string                            `json:"walletId"`
	StoredBalance     struct{ Amount, Currency string } `json:"storedBalance"`
	CalculatedBalance struct{ Amount, Currency string } `json:"calculatedBalance"`
	Difference        struct{ Amount, Currency string } `json:"difference"`
	Consistent        bool                              `json:"consistent"`
	CheckedEntries    int                               `json:"checkedEntries"`
}

func (w *wagering) reconcile(walletID string) reconciliation {
	w.t.Helper()

	resp, err := w.internal.Post(w.base+"/wallets/"+walletID+"/reconciliation", "application/json", strings.NewReader(""))
	if err != nil {
		w.t.Fatalf("POST reconciliation: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		w.t.Fatalf("POST reconciliation status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	var report reconciliation
	if err := json.NewDecoder(resp.Body).Decode(&report); err != nil {
		w.t.Fatalf("decode reconciliation: %v", err)
	}
	return report
}

func TestReconciliationAgreesAfterMixedOperationsAndChangesNothing(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "1000.00")

	api.bet(bet{externalID: "rec-bet", key: "provider-a:rec-bet", playerID: playerID, walletID: walletID, amount: "25.00"})
	api.bet(bet{externalID: "rec-win", key: "provider-a:rec-win", playerID: playerID, walletID: walletID, amount: "10.00", kind: "WIN"})
	api.bet(bet{externalID: "rec-loss", key: "provider-a:rec-loss", playerID: playerID, walletID: walletID, amount: "0.00", kind: "LOSS"})

	balanceBefore, versionBefore := api.wallet(walletID)
	entriesBefore := len(api.ledger(walletID))

	report := api.reconcile(walletID)
	if !report.Consistent {
		t.Errorf("consistent = false, want true (%+v)", report)
	}
	if report.StoredBalance.Amount != "985.00" || report.CalculatedBalance.Amount != "985.00" {
		t.Errorf("balances = %s / %s, want 985.00 both", report.StoredBalance.Amount, report.CalculatedBalance.Amount)
	}
	if report.Difference.Amount != "0.00" || report.Difference.Currency != "BRL" {
		t.Errorf("difference = %s %s, want 0.00 BRL", report.Difference.Amount, report.Difference.Currency)
	}
	// The opening, the bet and the win; the LOSS moves nothing.
	if report.CheckedEntries != 3 {
		t.Errorf("checkedEntries = %d, want 3", report.CheckedEntries)
	}

	balanceAfter, versionAfter := api.wallet(walletID)
	if balanceAfter != balanceBefore || versionAfter != versionBefore {
		t.Errorf("wallet moved: %s v%d -> %s v%d", balanceBefore, versionBefore, balanceAfter, versionAfter)
	}
	if entriesAfter := len(api.ledger(walletID)); entriesAfter != entriesBefore {
		t.Errorf("ledger entries = %d, want %d", entriesAfter, entriesBefore)
	}
}

func TestReconciliationReportsADivergenceAndCountsIt(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "100.00")

	ctx := context.Background()
	conn := api.connect()
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, `UPDATE wallets SET balance_minor = balance_minor - 500 WHERE id = $1`, walletID); err != nil {
		t.Fatalf("diverge the wallet: %v", err)
	}

	before := metrics.ReconciliationDivergences.Value()
	report := api.reconcile(walletID)

	if report.Consistent {
		t.Errorf("consistent = true, want false (%+v)", report)
	}
	if report.Difference.Amount != "-5.00" {
		t.Errorf("difference = %s, want -5.00", report.Difference.Amount)
	}
	if got := metrics.ReconciliationDivergences.Value(); got != before+1 {
		t.Errorf("divergence metric = %d, want %d", got, before+1)
	}
}

// A movement committing while the balance and the ledger are read must not
// look like a divergence: both sides come from one snapshot.
func TestReconciliationDoesNotSeeAConcurrentMovement(t *testing.T) {
	api := startWagering(t)
	playerID := uuid.NewV7().String()
	walletID := api.openWallet(playerID, "1000.00")

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := range 20 {
			id := "rec-race-" + string(rune('a'+i))
			api.bet(bet{externalID: id, key: "provider-a:" + id, playerID: playerID, walletID: walletID, amount: "1.00"})
		}
	}()

	for range 20 {
		if report := api.reconcile(walletID); !report.Consistent {
			t.Errorf("consistent = false during a concurrent movement: %+v", report)
		}
	}
	wg.Wait()

	if report := api.reconcile(walletID); !report.Consistent || report.StoredBalance.Amount != "980.00" {
		t.Errorf("final reconciliation = %+v, want consistent at 980.00", report)
	}
}
