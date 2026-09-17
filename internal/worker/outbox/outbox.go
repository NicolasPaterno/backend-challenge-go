// Package outbox drains the outbox table onto the outbound queue (§11). It runs
// in every instance; the claim is what keeps two of them off the same row.
package outbox

import (
	"context"
	"log/slog"
	"time"

	"go.uber.org/fx"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
)

// Event is one outbox row on its way out. Payload is the envelope exactly as
// the originating commit wrote it, never rebuilt, so a republication is
// byte-identical and keeps its EventID (§11).
type Event struct {
	EventID     uuid.UUID
	AggregateID uuid.UUID
	Payload     []byte
}

type Publisher interface {
	Publish(ctx context.Context, e Event) error
}

// Store claims due rows and records each outcome in the same transaction as the
// claim, so a publisher that dies releases its rows without leaving a lease
// behind. publish is called with the claim held; a failure is backed off, not
// propagated.
type Store interface {
	Drain(ctx context.Context, limit int, publish func(context.Context, Event) error) (published int, err error)
}

type Worker struct {
	store     Store
	publisher Publisher
	logger    *slog.Logger
	cfg       config.Config

	stop chan struct{}
	done chan struct{}
}

func New(lc fx.Lifecycle, store Store, publisher Publisher, cfg config.Config, logger *slog.Logger) *Worker {
	w := &Worker{
		store:     store,
		publisher: publisher,
		logger:    logger.With(slog.String("component", "outbox-publisher")),
		cfg:       cfg,
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
	}

	// Cancelling this is the last resort: OnStop first asks the loop to stop
	// fetching and lets the cycle in flight finish (§4).
	ctx, cancel := context.WithCancel(context.Background())

	lc.Append(fx.Hook{
		OnStart: func(context.Context) error {
			go w.run(ctx)
			w.logger.Info("outbox publisher started",
				slog.Duration("poll_interval", cfg.OutboxPollInterval),
				slog.Int("batch_size", cfg.OutboxBatchSize))
			return nil
		},
		OnStop: func(ctx context.Context) error {
			close(w.stop)
			select {
			case <-w.done:
				w.logger.Info("outbox publisher stopped")
			case <-ctx.Done():
				cancel()
				<-w.done
				w.logger.Warn("outbox publisher stopped past its deadline, in-flight claims released")
			}
			cancel()
			return nil
		},
	})

	return w
}

func (w *Worker) run(ctx context.Context) {
	defer close(w.done)

	ticker := time.NewTicker(w.cfg.OutboxPollInterval)
	defer ticker.Stop()

	for {
		// A full batch means more rows are already due, so the next cycle does
		// not wait out the tick.
		if w.cycle(ctx) < w.cfg.OutboxBatchSize {
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
	ctx, cancel := context.WithTimeout(ctx, w.cfg.OutboxPublishWindow)
	defer cancel()

	published, err := w.store.Drain(ctx, w.cfg.OutboxBatchSize, w.publisher.Publish)
	if err != nil && ctx.Err() == nil {
		w.logger.Error("outbox drain failed", slog.Any("error", err))
	}
	return published
}

var Module = fx.Module("outbox-worker",
	fx.Provide(New),
	fx.Invoke(func(*Worker) {}),
)
