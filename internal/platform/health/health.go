// Package health serves the public health checks (§9). Liveness must stay 200
// while dependencies are degraded; readiness reports PostgreSQL and SQS.
package health

import (
	"net/http"

	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/httpserver"
)

func NewLiveRoute() httpserver.Route {
	return httpserver.Route{
		Pattern: "GET /health/live",
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		}),
	}
}

var Module = fx.Module("health", fx.Provide(
	httpserver.AsRoute(NewLiveRoute),
	httpserver.AsRoute(NewReadyRoute),
))
