package walletapp

import (
	"context"
	"time"

	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wallet"
)

// LedgerCursor is the sort key of a page's last entry. Paging by it rather than
// by offset is what keeps a concurrent insert from shifting a boundary (§9).
type LedgerCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

const (
	LedgerDefaultLimit = 50
	LedgerMaxLimit     = 200
)

// Bounded here rather than in the handler so every transport pages the same way.
func LedgerLimit(limit int) int {
	switch {
	case limit <= 0:
		return LedgerDefaultLimit
	case limit > LedgerMaxLimit:
		return LedgerMaxLimit
	}
	return limit
}

type LedgerParams struct {
	WalletID uuid.UUID
	After    *LedgerCursor
	Limit    int
}

// An empty page is ambiguous — an exhausted ledger or an unknown wallet — so it
// is the one case worth a second query.
func (s *Service) Ledger(ctx context.Context, p LedgerParams) ([]*wallet.LedgerEntry, error) {
	p.Limit = LedgerLimit(p.Limit)

	entries, err := s.repo.Ledger(ctx, p.WalletID, p.After, p.Limit)
	if err != nil {
		return nil, err
	}
	if len(entries) == 0 {
		if _, err := s.repo.ByID(ctx, p.WalletID); err != nil {
			return nil, err
		}
	}
	return entries, nil
}
