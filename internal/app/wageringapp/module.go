package wageringapp

import "go.uber.org/fx"

var Module = fx.Module("app-wagering",
	fx.Provide(
		NewService,
		fx.Annotate(func() UUIDv7 { return UUIDv7{} }, fx.As(new(IDGenerator))),
	),
)
