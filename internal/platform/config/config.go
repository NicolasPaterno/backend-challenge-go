// Package config loads and validates the process configuration from the
// environment, failing startup before any dependency is constructed.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Env               string
	HTTPAddr          string
	ReadHeaderTimeout time.Duration
	ShutdownTimeout   time.Duration
	StartupTimeout    time.Duration
	DatabaseURL       string
	DBMaxConns        int32
	DBMinConns        int32
	DBLockTimeout     time.Duration
	LogLevel          string

	// The two differ inside Compose: the host and the api container reach
	// Keycloak under different names, and only the issuer is claimed by tokens.
	OIDCIssuerURL    string
	OIDCDiscoveryURL string
	OIDCAudience     string

	AWSRegion   string
	SQSEndpoint string

	WagerQueueURL  string
	WagerDLQURL    string
	EventsQueueURL string

	OutboxPollInterval  time.Duration
	OutboxPublishWindow time.Duration
	OutboxBatchSize     int

	WorkerDrainTimeout time.Duration

	ReferencePollInterval time.Duration
	ReferenceBatchSize    int
	ReferenceTTL          time.Duration
}

// Load reports every problem at once, so a misconfigured deployment is not
// fixed one variable per restart.
func Load() (Config, error) {
	var errs []error
	fail := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf(format, args...))
	}

	cfg := Config{
		Env:      envOr("APP_ENV", "local"),
		HTTPAddr: envOr("HTTP_ADDR", ":8080"),
		LogLevel: strings.ToLower(envOr("LOG_LEVEL", "info")),
	}

	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		fail("DATABASE_URL is required")
	}

	cfg.OIDCIssuerURL = os.Getenv("OIDC_ISSUER_URL")
	if cfg.OIDCIssuerURL == "" {
		fail("OIDC_ISSUER_URL is required")
	}
	cfg.OIDCAudience = os.Getenv("OIDC_AUDIENCE")
	if cfg.OIDCAudience == "" {
		fail("OIDC_AUDIENCE is required")
	}
	cfg.OIDCDiscoveryURL = envOr("OIDC_DISCOVERY_URL", cfg.OIDCIssuerURL)

	cfg.AWSRegion = envOr("AWS_REGION", "us-east-1")
	// LocalStack's address; empty means the real service.
	cfg.SQSEndpoint = os.Getenv("SQS_ENDPOINT")

	// The brief names all three: the inbound queue, its dead-letter queue — which the
	// consumer sends permanent failures to directly — and the outbound one.
	for _, required := range []struct {
		key  string
		into *string
	}{
		{"SQS_WAGER_TRANSACTIONS_QUEUE_URL", &cfg.WagerQueueURL},
		{"SQS_WAGER_TRANSACTIONS_DLQ_URL", &cfg.WagerDLQURL},
		{"SQS_EVENTS_QUEUE_URL", &cfg.EventsQueueURL},
	} {
		if *required.into = os.Getenv(required.key); *required.into == "" {
			fail("%s is required", required.key)
		}
	}

	if _, _, err := net.SplitHostPort(cfg.HTTPAddr); err != nil {
		fail("HTTP_ADDR must be a host:port listen address, got %q", cfg.HTTPAddr)
	}
	switch cfg.LogLevel {
	case "debug", "info", "warn", "error":
	default:
		fail("LOG_LEVEL must be one of debug, info, warn, error, got %q", cfg.LogLevel)
	}

	var readHeaderErr, shutdownErr, startupErr error
	cfg.ReadHeaderTimeout, readHeaderErr = durationEnv("HTTP_READ_HEADER_TIMEOUT", 5*time.Second)
	cfg.ShutdownTimeout, shutdownErr = durationEnv("SHUTDOWN_TIMEOUT", 15*time.Second)
	cfg.StartupTimeout, startupErr = durationEnv("STARTUP_TIMEOUT", 15*time.Second)
	// Each worker's own share of the shutdown, so one draining slowly cannot
	// spend SHUTDOWN_TIMEOUT and leave the HTTP server no time to finish its
	// own in-flight work. The effective wait is the smaller of this and
	// whatever the shutdown has left, so a value above SHUTDOWN_TIMEOUT simply
	// has no effect.
	var drainErr error
	cfg.WorkerDrainTimeout, drainErr = durationEnv("WORKER_DRAIN_TIMEOUT", 5*time.Second)
	errs = append(errs, drainErr)
	errs = append(errs, readHeaderErr, shutdownErr, startupErr)

	var lockTimeoutErr error
	cfg.DBLockTimeout, lockTimeoutErr = durationEnv("DB_LOCK_TIMEOUT", 3*time.Second)
	errs = append(errs, lockTimeoutErr)

	var pollErr, windowErr error
	cfg.OutboxPollInterval, pollErr = durationEnv("OUTBOX_POLL_INTERVAL", time.Second)
	// Bounds one publish cycle, and with it how long a claimed row stays locked
	// by a publisher that hangs instead of dying.
	cfg.OutboxPublishWindow, windowErr = durationEnv("OUTBOX_PUBLISH_WINDOW", 10*time.Second)
	errs = append(errs, pollErr, windowErr)

	batchSize, batchErr := intEnv("OUTBOX_BATCH_SIZE", 100, 1)
	errs = append(errs, batchErr)
	cfg.OutboxBatchSize = batchSize

	var referencePollErr, ttlErr error
	cfg.ReferencePollInterval, referencePollErr = durationEnv("REFERENCE_POLL_INTERVAL", time.Second)
	// The brief asks for a maximum attempt count or a TTL; this is the TTL, measured
	// from the operation's created_at. On expiry the reversal is REJECTED with
	// REFERENCE_NOT_FOUND.
	cfg.ReferenceTTL, ttlErr = durationEnv("REFERENCE_TTL", 24*time.Hour)
	errs = append(errs, referencePollErr, ttlErr)

	referenceBatch, referenceBatchErr := intEnv("REFERENCE_BATCH_SIZE", 100, 1)
	errs = append(errs, referenceBatchErr)
	cfg.ReferenceBatchSize = referenceBatch

	maxConns, maxErr := intEnv("DB_MAX_CONNS", 10, 1)
	minConns, minErr := intEnv("DB_MIN_CONNS", 1, 0)
	errs = append(errs, maxErr, minErr)
	cfg.DBMaxConns, cfg.DBMinConns = int32(maxConns), int32(minConns)

	// Guarded: unparsed values are zero and would report a bound the operator
	// never wrote.
	if maxErr == nil && minErr == nil && minConns > maxConns {
		fail("DB_MIN_CONNS (%d) must not exceed DB_MAX_CONNS (%d)", minConns, maxConns)
	}

	if err := errors.Join(errs...); err != nil {
		return Config{}, fmt.Errorf("invalid configuration: %w", err)
	}
	return cfg, nil
}

// envOr treats an empty value as unset, because Compose and .env files render
// an omitted variable that way.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be a duration such as 15s, got %q", key, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", key, raw)
	}
	return d, nil
}

func intEnv(key string, fallback, min int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer, got %q", key, raw)
	}
	if n < min {
		return 0, fmt.Errorf("%s must be at least %d, got %d", key, min, n)
	}
	return n, nil
}
