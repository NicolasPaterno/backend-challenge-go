// Command api runs the HTTP API process.
package main

import (
	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/adapter/httpapi"
	pgadapter "github.com/NicolasPaterno/backend-challenge-go/internal/adapter/postgres"
	"github.com/NicolasPaterno/backend-challenge-go/internal/app/wageringapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/app/walletapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/auth"
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
		auth.Module,
		postgres.Module,
		httpserver.Module,
		health.Module,
		pgadapter.Module,
		walletapp.Module,
		wageringapp.Module,
		httpapi.Module,
		fx.WithLogger(logging.FxLogger),
	)
}

func main() {
	fx.New(options()).Run()
}
