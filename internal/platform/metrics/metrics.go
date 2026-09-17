// Package metrics holds the counters §12 requires. expvar keeps them in the
// standard library and in one place; 16 exposes them and adds the rest.
package metrics

import "expvar"

var ReconciliationDivergences = expvar.NewInt("reconciliation_divergences")
