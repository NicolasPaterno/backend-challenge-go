package walletapp

import (
	"context"
	"testing"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
)

func TestReconcileReportsStoredMinusRebuilt(t *testing.T) {
	tests := []struct {
		name                       string
		stored, calculated, differ string
		consistent                 bool
	}{
		{"agreeing", "975.00", "975.00", "0.00", true},
		{"stored ahead", "1000.00", "975.00", "25.00", false},
		{"stored behind", "950.00", "975.00", "-25.00", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stored, err1 := money.Parse(tt.stored, money.BRL)
			calculated, err2 := money.Parse(tt.calculated, money.BRL)
			if err1 != nil || err2 != nil {
				t.Fatalf("Parse() errors = %v, %v", err1, err2)
			}

			repo := &recordingRepo{stored: stored, calculated: calculated, entries: 2}
			report, err := NewService(repo, &sequentialIDs{}).Reconcile(context.Background(), uuid.NewV7())
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}

			if got := report.Difference.String(); got != tt.differ+" BRL" {
				t.Errorf("difference = %s, want %s BRL", got, tt.differ)
			}
			if report.Consistent != tt.consistent {
				t.Errorf("consistent = %v, want %v", report.Consistent, tt.consistent)
			}
			if report.CheckedEntries != 2 {
				t.Errorf("checkedEntries = %d, want 2", report.CheckedEntries)
			}
		})
	}
}
