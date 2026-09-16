//go:build integration

// Package testsupport starts the real infrastructure the integration tests run
// against; §13 forbids replacing it with mocks.
package testsupport

import (
	"context"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"

	"github.com/NicolasPaterno/backend-challenge-go/migrations"
)

// PostgresImage matches the one Compose runs.
const PostgresImage = "postgres:17-alpine"

// Postgres starts an empty container, terminated when the test ends. Use it
// when the test is about the schema itself; else use PostgresMigrated.
func Postgres(t *testing.T) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := postgres.Run(ctx, PostgresImage,
		postgres.WithDatabase("wagering"),
		postgres.WithUsername("wagering"),
		postgres.WithPassword("wagering"),
		postgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if err := testcontainers.TerminateContainer(container); err != nil {
			t.Logf("terminate postgres container: %v", err)
		}
	})

	databaseURL, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("read postgres connection string: %v", err)
	}

	return databaseURL
}

func PostgresMigrated(t *testing.T) string {
	t.Helper()

	databaseURL := Postgres(t)
	if err := migrations.Up(databaseURL); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return databaseURL
}
