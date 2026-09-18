// Package metrics holds the counters the brief requires. expvar keeps them in the
// standard library — no registry of our own, no dependency — and serves them
// as JSON at /metrics.
package metrics

import (
	"expvar"
	"time"

	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/httpserver"
)

var (
	// Keyed by the transaction's final status: PROCESSED, REJECTED,
	// PENDING_REFERENCE.
	Outcomes = expvar.NewMap("wager_outcomes")

	Duplicates           = expvar.NewInt("wager_duplicates")
	ConcurrencyConflicts = expvar.NewInt("wallet_concurrency_conflicts")
	ReferenceRetries     = expvar.NewInt("reference_retries")
	DeadLettered         = expvar.NewInt("sqs_dead_lettered")

	ReconciliationDivergences = expvar.NewInt("reconciliation_divergences")

	// Count and total, because expvar has no histogram: the average is the
	// division, and a percentile needs a real metrics backend.
	// ponytail: swap for a Prometheus histogram if percentiles are wanted.
	Processing = expvar.NewMap("wager_processing")
)

func ObserveProcessing(d time.Duration) {
	Processing.Add("count", 1)
	Processing.Add("total_ms", d.Milliseconds())
}

// Public, like the health checks: the values name no player, no wallet and no
// amount.
func NewRoute() httpserver.Route {
	return httpserver.Route{Pattern: "GET /metrics", Handler: expvar.Handler()}
}

var Module = fx.Module("metrics", fx.Provide(httpserver.AsRoute(NewRoute)))
