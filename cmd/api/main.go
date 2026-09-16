// Command api runs the HTTP API process.
package main

import (
	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/health"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/httpserver"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/logging"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/postgres"
)

func options() fx.Option {
	return fx.Options(
		config.Module,
		logging.Module,
		postgres.Module,
		httpserver.Module,
		health.Module,
		fx.WithLogger(logging.FxLogger),
	)
}

func main() {
	fx.New(options()).Run()
}
