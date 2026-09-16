package postgres

import (
	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/app/walletapp"
)

var Module = fx.Module("postgres-adapter",
	fx.Provide(
		fx.Annotate(NewWalletRepository, fx.As(new(walletapp.Repository))),
	),
)
