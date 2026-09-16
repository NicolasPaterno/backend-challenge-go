package httpapi

import (
	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/httpserver"
)

var Module = fx.Module("http-adapter",
	fx.Provide(
		NewWalletHandler,
		httpserver.AsRoute(NewOpenWalletRoute),
		httpserver.AsRoute(NewGetWalletRoute),
		httpserver.AsRoute(NewGetWalletLedgerRoute),
	),
)
