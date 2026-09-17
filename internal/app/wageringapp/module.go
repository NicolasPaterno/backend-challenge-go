package wageringapp

import (
	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
)

var Module = fx.Module("app-wagering",
	fx.Provide(
		NewService,
		func(cfg config.Config) ReferenceTTL { return ReferenceTTL(cfg.ReferenceTTL) },
		fx.Annotate(func() UUIDv7 { return UUIDv7{} }, fx.As(new(IDGenerator))),
	),
)
