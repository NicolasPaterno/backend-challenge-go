// Package logging provides the process-wide JSON logger.
package logging

import (
	"context"
	"log/slog"
	"os"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/correlation"
)

func New(cfg config.Config) *slog.Logger {
	// config rejected anything slog cannot parse; the zero value is INFO.
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(correlated{handler}).With(slog.String("env", cfg.Env))
}

// correlated stamps every line logged with a context — the *Context methods of
// slog — with the id that request or message is running under, so no call
// site has to remember to pass it.
type correlated struct{ slog.Handler }

func (h correlated) Handle(ctx context.Context, record slog.Record) error {
	if id, ok := correlation.FromContext(ctx); ok {
		record.AddAttrs(slog.String("correlationId", id.String()))
	}
	return h.Handler.Handle(ctx, record)
}

func (h correlated) WithAttrs(attrs []slog.Attr) slog.Handler {
	return correlated{h.Handler.WithAttrs(attrs)}
}

func (h correlated) WithGroup(name string) slog.Handler {
	return correlated{h.Handler.WithGroup(name)}
}

var Module = fx.Module("logging", fx.Provide(New))

func FxLogger(logger *slog.Logger) fxevent.Logger {
	return &fxevent.SlogLogger{Logger: logger.With(slog.String("component", "fx"))}
}
