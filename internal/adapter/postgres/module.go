package postgres

import (
	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/app/wageringapp"
	"github.com/NicolasPaterno/backend-challenge-go/internal/app/walletapp"
)

var Module = fx.Module("postgres-adapter",
	fx.Provide(
		fx.Annotate(NewWalletRepository, fx.As(new(walletapp.Repository))),
		fx.Annotate(NewWagerRepository, fx.As(new(wageringapp.Repository))),
	),
)
