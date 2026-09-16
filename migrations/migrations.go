// Package migrations embeds the versioned SQL migrations and applies them in
// both directions. The migrate binary and the integration tests are its users.
package migrations

import (
	"embed"
	"errors"
	"fmt"
	"strings"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // registers pgx5://
	"github.com/golang-migrate/migrate/v4/source/iofs"
)

//go:embed *.sql
var files embed.FS

func Up(databaseURL string) error { return run(databaseURL, (*migrate.Migrate).Up) }

func Down(databaseURL string) error { return run(databaseURL, (*migrate.Migrate).Down) }

// Steps moves n migrations forward (n > 0) or backward (n < 0).
func Steps(databaseURL string, n int) error {
	return run(databaseURL, func(m *migrate.Migrate) error { return m.Steps(n) })
}

// Version reports the current schema version, and whether a previous run
// failed part way and needs manual attention.
func Version(databaseURL string) (version uint, dirty bool, err error) {
	m, err := newMigrate(databaseURL)
	if err != nil {
		return 0, false, err
	}
	defer m.Close()

	version, dirty, err = m.Version()
	if errors.Is(err, migrate.ErrNilVersion) {
		return 0, false, nil
	}
	return version, dirty, err
}

func run(databaseURL string, action func(*migrate.Migrate) error) error {
	m, err := newMigrate(databaseURL)
	if err != nil {
		return err
	}
	defer m.Close()

	if err := action(m); err != nil && !errors.Is(err, migrate.ErrNoChange) {
		return fmt.Errorf("run migrations: %w", err)
	}
	return nil
}

func newMigrate(databaseURL string) (*migrate.Migrate, error) {
	source, err := iofs.New(files, ".")
	if err != nil {
		return nil, fmt.Errorf("open embedded migrations: %w", err)
	}
	m, err := migrate.NewWithSourceInstance("iofs", source, pgxURL(databaseURL))
	if err != nil {
		return nil, fmt.Errorf("connect migrator: %w", err)
	}
	return m, nil
}

// pgxURL rewrites postgres:// to the scheme golang-migrate registers, so one
// DATABASE_URL serves both the migrator and pgxpool.
func pgxURL(databaseURL string) string {
	for _, scheme := range []string{"postgres://", "postgresql://"} {
		if after, ok := strings.CutPrefix(databaseURL, scheme); ok {
			return "pgx5://" + after
		}
	}
	return databaseURL
}
