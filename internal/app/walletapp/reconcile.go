package walletapp

import (
	"context"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/money"
)

// Reconciliation is the report: the stored balance against the one rebuilt
// from the ledger, and the difference between them. Nothing here writes.
type Reconciliation struct {
	WalletID       uuid.UUID
	Stored         money.Money
	Calculated     money.Money
	Difference     money.Money
	Consistent     bool
	CheckedEntries int
}

func (s *Service) Reconcile(ctx context.Context, walletID uuid.UUID) (Reconciliation, error) {
	stored, calculated, entries, err := s.repo.Reconcile(ctx, walletID)
	if err != nil {
		return Reconciliation{}, err
	}

	// stored minus rebuilt, so a shortfall reads negative.
	difference, err := stored.Sub(calculated)
	if err != nil {
		return Reconciliation{}, err
	}

	return Reconciliation{
		WalletID:       walletID,
		Stored:         stored,
		Calculated:     calculated,
		Difference:     difference,
		Consistent:     difference.IsZero(),
		CheckedEntries: entries,
	}, nil
}
