// Package postgres builds the pgx pool and binds it to the Fx lifecycle.
package postgres

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
)

func NewPool(lc fx.Lifecycle, cfg config.Config, logger *slog.Logger) (*pgxpool.Pool, error) {
	poolCfg, err := pgxpool.ParseConfig(cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	poolCfg.MaxConns = cfg.DBMaxConns
	poolCfg.MinConns = cfg.DBMinConns

	pool, err := pgxpool.NewWithConfig(context.Background(), poolCfg)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, cfg.StartupTimeout)
			defer cancel()

			// pgxpool connects lazily; without this ping an unreachable
			// database would surface on the first request instead of at boot.
			if err := pool.Ping(ctx); err != nil {
				pool.Close()
				return fmt.Errorf("ping postgres: %w", err)
			}
			logger.Info("postgres pool ready",
				slog.String("database", poolCfg.ConnConfig.Database),
				slog.Int("max_conns", int(poolCfg.MaxConns)),
			)
			return nil
		},
		OnStop: func(context.Context) error {
			pool.Close()
			logger.Info("postgres pool closed")
			return nil
		},
	})

	return pool, nil
}

// The Invoke forces construction: Fx builds lazily, so a process whose
// components do not yet depend on the pool would never check the database.
var Module = fx.Module("postgres",
	fx.Provide(NewPool),
	fx.Invoke(func(*pgxpool.Pool) {}),
)
