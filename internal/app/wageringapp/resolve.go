package wageringapp

import (
	"context"
	"errors"
	"fmt"

	"github.com/NicolasPaterno/backend-challenge-go/internal/domain/wagering"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/metrics"
)

// ResolveDue re-runs the reversals whose wait has come round, one SQL
// transaction each so a wallet held by another writer delays only its own
// (§8). Every record is attempted even when one fails: a record that errors
// commits nothing, so leaving the batch on the first failure would let it block
// the queue behind it forever.
func (s *Service) ResolveDue(ctx context.Context, limit int) (int, error) {
	due, err := s.repo.DuePendingReferences(ctx, limit)
	if err != nil {
		return 0, err
	}

	metrics.ReferenceRetries.Add(int64(len(due)))

	var (
		resolved int
		errs     []error
	)
	for _, id := range due {
		now := s.now()
		resume := func(t *wagering.WagerTransaction) Decide { return s.decide(ctx, t, now) }
		if err := s.repo.Resume(ctx, id, resume); err != nil {
			errs = append(errs, fmt.Errorf("resume %s: %w", id, err))
			continue
		}
		resolved++
	}
	return resolved, errors.Join(errs...)
}
