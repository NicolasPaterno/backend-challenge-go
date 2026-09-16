// Package logging provides the process-wide JSON logger (spec §12).
package logging

import (
	"log/slog"
	"os"

	"go.uber.org/fx"
	"go.uber.org/fx/fxevent"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
)

func New(cfg config.Config) *slog.Logger {
	// config rejected anything slog cannot parse; the zero value is INFO.
	var level slog.Level
	_ = level.UnmarshalText([]byte(cfg.LogLevel))

	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler).With(slog.String("env", cfg.Env))
}

var Module = fx.Module("logging", fx.Provide(New))

// FxLogger routes Fx's own lifecycle events through the JSON logger. Apply it
// with fx.WithLogger at the root of the application, not inside a module.
func FxLogger(logger *slog.Logger) fxevent.Logger {
	return &fxevent.SlogLogger{Logger: logger.With(slog.String("component", "fx"))}
}
