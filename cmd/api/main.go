// Command api runs the HTTP API process.
package main

import (
	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/adapter/httpapi"
	pgadapter "github.com/NicolasPaterno/backend-challenge-go/internal/adapter/postgres"
	"github.com/NicolasPaterno/backend-challenge-go/internal/adapter/sqs"
	"github.com/NicolasPaterno/backend-challenge-go/internal/app/wageringapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/app/walletapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/auth"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/health"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/httpserver"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/logging"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/metrics"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/postgres"
	"github.com/NicolasPaterno/backend-challenge-go/internal/worker/outbox"
	"github.com/NicolasPaterno/backend-challenge-go/internal/worker/reference"
)

func options() fx.Option {
	return fx.Options(
		config.Module,
		logging.Module,
		auth.Module,
		postgres.Module,
		httpserver.Module,
		health.Module,
		metrics.Module,
		pgadapter.Module,
		walletapp.Module,
		wageringapp.Module,
		httpapi.Module,
		sqs.Module,
		outbox.Module,
		reference.Module,
		// The worker's port is bound here rather than in either package: the app
		// layer must not import a worker, and the worker must not import the app.
		fx.Provide(
			func(s *wageringapp.Service) reference.Resolver { return s },
			func(s *wageringapp.Service) sqs.Submitter { return s },
		),
		fx.WithLogger(logging.FxLogger),
	)
}

func main() {
	fx.New(options()).Run()
}
