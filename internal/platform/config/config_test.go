package config_test

import (
	"strings"
	"testing"
	"time"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
)

// setEnv clears every key the loader reads, so an ambient variable cannot make
// a test pass or fail by accident, then fills the auth settings every case but
// the auth ones takes for granted. A case that wants one missing sets it to "".
func setEnv(t *testing.T, env map[string]string) {
	t.Helper()
	for _, key := range []string{
		"APP_ENV", "HTTP_ADDR", "LOG_LEVEL", "DATABASE_URL",
		"HTTP_READ_HEADER_TIMEOUT", "SHUTDOWN_TIMEOUT", "STARTUP_TIMEOUT",
		"DB_MAX_CONNS", "DB_MIN_CONNS",
		"OIDC_ISSUER_URL", "OIDC_DISCOVERY_URL", "OIDC_AUDIENCE",
		"AWS_REGION", "AWS_ENDPOINT_URL", "OUTBOX_QUEUE_URL",
		"OUTBOX_POLL_INTERVAL", "OUTBOX_PUBLISH_WINDOW", "OUTBOX_BATCH_SIZE",
	} {
		t.Setenv(key, "")
	}
	t.Setenv("OIDC_ISSUER_URL", "http://localhost:8081/realms/wagering")
	t.Setenv("OIDC_AUDIENCE", "wagering-api")
	t.Setenv("OUTBOX_QUEUE_URL", "http://localhost:4566/000000000000/wager-events.fifo")
	for key, value := range env {
		t.Setenv(key, value)
	}
}

func TestLoadAppliesDefaults(t *testing.T) {
	setEnv(t, map[string]string{"DATABASE_URL": "postgres://user:pass@localhost:5432/db"})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.Env != "local" {
		t.Errorf("Env = %q, want %q", cfg.Env, "local")
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr = %q, want %q", cfg.HTTPAddr, ":8080")
	}
	if cfg.ShutdownTimeout != 15*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 15s", cfg.ShutdownTimeout)
	}
	if cfg.DBMaxConns != 10 || cfg.DBMinConns != 1 {
		t.Errorf("pool bounds = (%d, %d), want (1, 10)", cfg.DBMinConns, cfg.DBMaxConns)
	}
	if cfg.OutboxPollInterval != time.Second || cfg.OutboxBatchSize != 100 {
		t.Errorf("outbox defaults = (%v, %d), want (1s, 100)", cfg.OutboxPollInterval, cfg.OutboxBatchSize)
	}
	// The API is normally reachable under one name, so discovery follows the issuer.
	if cfg.OIDCDiscoveryURL != cfg.OIDCIssuerURL {
		t.Errorf("OIDCDiscoveryURL = %q, want it to default to the issuer %q",
			cfg.OIDCDiscoveryURL, cfg.OIDCIssuerURL)
	}
}

func TestLoadReadsOverrides(t *testing.T) {
	setEnv(t, map[string]string{
		"APP_ENV":            "production",
		"HTTP_ADDR":          "127.0.0.1:9000",
		"LOG_LEVEL":          "DEBUG",
		"DATABASE_URL":       "postgres://localhost/db",
		"SHUTDOWN_TIMEOUT":   "42s",
		"DB_MAX_CONNS":       "25",
		"DB_MIN_CONNS":       "5",
		"OIDC_DISCOVERY_URL": "http://keycloak:8080/realms/wagering",
	})

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load() error = %v, want nil", err)
	}

	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want %q (case is normalised)", cfg.LogLevel, "debug")
	}
	if cfg.HTTPAddr != "127.0.0.1:9000" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.ShutdownTimeout != 42*time.Second {
		t.Errorf("ShutdownTimeout = %v, want 42s", cfg.ShutdownTimeout)
	}
	if cfg.DBMaxConns != 25 || cfg.DBMinConns != 5 {
		t.Errorf("pool bounds = (%d, %d), want (5, 25)", cfg.DBMinConns, cfg.DBMaxConns)
	}
	if cfg.OIDCDiscoveryURL != "http://keycloak:8080/realms/wagering" {
		t.Errorf("OIDCDiscoveryURL = %q, want the override", cfg.OIDCDiscoveryURL)
	}
}

func TestLoadRejectsInvalidEnvironment(t *testing.T) {
	tests := map[string]struct {
		env  map[string]string
		want string
	}{
		"missing database url": {
			env:  map[string]string{},
			want: "DATABASE_URL is required",
		},
		"missing issuer": {
			env: map[string]string{
				"DATABASE_URL":    "postgres://localhost/db",
				"OIDC_ISSUER_URL": "",
			},
			want: "OIDC_ISSUER_URL is required",
		},
		"missing outbox queue": {
			env: map[string]string{
				"DATABASE_URL":     "postgres://localhost/db",
				"OUTBOX_QUEUE_URL": "",
			},
			want: "OUTBOX_QUEUE_URL is required",
		},
		"missing audience": {
			env: map[string]string{
				"DATABASE_URL":  "postgres://localhost/db",
				"OIDC_AUDIENCE": "",
			},
			want: "OIDC_AUDIENCE is required",
		},
		"unparseable duration": {
			env: map[string]string{
				"DATABASE_URL":     "postgres://localhost/db",
				"SHUTDOWN_TIMEOUT": "soon",
			},
			want: "SHUTDOWN_TIMEOUT must be a duration",
		},
		"non-positive duration": {
			env: map[string]string{
				"DATABASE_URL":    "postgres://localhost/db",
				"STARTUP_TIMEOUT": "0s",
			},
			want: "STARTUP_TIMEOUT must be positive",
		},
		"unparseable int": {
			env: map[string]string{
				"DATABASE_URL": "postgres://localhost/db",
				"DB_MAX_CONNS": "many",
			},
			want: "DB_MAX_CONNS must be an integer",
		},
		"min above max": {
			env: map[string]string{
				"DATABASE_URL": "postgres://localhost/db",
				"DB_MAX_CONNS": "2",
				"DB_MIN_CONNS": "8",
			},
			want: "must not exceed DB_MAX_CONNS",
		},
		"malformed listen address": {
			env: map[string]string{
				"DATABASE_URL": "postgres://localhost/db",
				"HTTP_ADDR":    "8080",
			},
			want: "HTTP_ADDR must be a host:port",
		},
		"unknown log level": {
			env: map[string]string{
				"DATABASE_URL": "postgres://localhost/db",
				"LOG_LEVEL":    "verbose",
			},
			want: "LOG_LEVEL must be one of",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			setEnv(t, tc.env)

			_, err := config.Load()
			if err == nil {
				t.Fatalf("Load() error = nil, want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Load() error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// Regression: an unparseable integer used to also trip the range check on its
// zero value, reporting a bound the operator never wrote.
func TestLoadReportsOneProblemPerVariable(t *testing.T) {
	setEnv(t, map[string]string{
		"DATABASE_URL": "postgres://localhost/db",
		"DB_MAX_CONNS": "many",
	})

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error")
	}
	if got := strings.Count(err.Error(), "DB_MAX_CONNS"); got != 1 {
		t.Errorf("DB_MAX_CONNS mentioned %d times, want 1:\n%v", got, err)
	}
}

func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	setEnv(t, map[string]string{
		"LOG_LEVEL":        "loud",
		"SHUTDOWN_TIMEOUT": "later",
	})

	_, err := config.Load()
	if err == nil {
		t.Fatal("Load() error = nil, want an error")
	}
	for _, want := range []string{"DATABASE_URL", "LOG_LEVEL", "SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Load() error = %v, want it to mention %s", err, want)
		}
	}
}
