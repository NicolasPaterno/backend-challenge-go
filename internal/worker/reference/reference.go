// Package reference retries the reversals waiting for the operation they undo
// (§7). It runs in every instance; the claim inside Resolver is what keeps two
// of them off the same record.
package reference

import (
	"context"
	"log/slog"
	"time"

	"go.uber.org/fx"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/correlation"
)

// Resolver re-runs the records whose wait has come round and reports how many
// it got through. The reversal rules, the backoff and the TTL live behind it;
// this package owns only when to ask.
type Resolver interface {
	ResolveDue(ctx context.Context, limit int) (resolved int, err error)
}

type Worker struct {
	resolver Resolver
	logger   *slog.Logger
	cfg      config.Config

	stop chan struct{}
	done chan struct{}
}

func New(lc fx.Lifecycle, resolver Resolver, cfg config.Config, logger *slog.Logger) *Worker {
	w := &Worker{
		resolver: resolver,
		logger:   logger.With(slog.String("component", "reference-resolver")),
		cfg:      cfg,
		stop:     make(chan struct{}),
		done:     make(chan struct{}),
	}

	// Cancelling this is the last resort: OnStop first asks the loop to stop
	// fetching and lets the cycle in flight finish (§4).
	ctx, cancel := context.WithCancel(context.Background())

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go w.run(ctx)
			w.logger.Info("reference resolver started",
				slog.Duration("poll_interval", cfg.ReferencePollInterval),
				slog.Duration("ttl", cfg.ReferenceTTL),
				slog.Int("batch_size", cfg.ReferenceBatchSize))
			return nil
		},
		OnStop: func(ctx context.Context) error {
			close(w.stop)

			// Bounded by the worker's own share, not by the whole shutdown (§4).
			drain, giveUp := context.WithTimeout(ctx, cfg.WorkerDrainTimeout)
			defer giveUp()

			select {
			case <-w.done:
				w.logger.Info("reference resolver stopped")
			case <-drain.Done():
				cancel()
				<-w.done
				w.logger.Warn("reference resolver stopped past its deadline, in-flight claims released")
			}
			cancel()
			return nil
		},
	})

	return w
}

func (w *Worker) run(ctx context.Context) {
	defer close(w.done)

	ticker := time.NewTicker(w.cfg.ReferencePollInterval)
	defer ticker.Stop()

	for {
		// A full batch means more records are already due, so the next cycle
		// does not wait out the tick.
		if w.cycle(ctx) < w.cfg.ReferenceBatchSize {
			select {
			case <-w.stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
		select {
		case <-w.stop:
			return
		case <-ctx.Done():
			return
		default:
		}
	}
}

func (w *Worker) cycle(ctx context.Context) int {
	// One id per cycle, so the lines of one sweep can be read together (§12).
	ctx = correlation.NewContext(ctx, uuid.NewV7())

	resolved, err := w.resolver.ResolveDue(ctx, w.cfg.ReferenceBatchSize)
	if err != nil && ctx.Err() == nil {
		w.logger.ErrorContext(ctx, "reference resolution failed", slog.Any("error", err))
	}
	return resolved
}

var Module = fx.Module("reference-worker",
	fx.Provide(New),
	fx.Invoke(func(*Worker) {}),
)
