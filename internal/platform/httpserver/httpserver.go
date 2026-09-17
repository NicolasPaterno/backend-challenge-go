// Package httpserver composes the public HTTP API and binds it to the Fx
// lifecycle. Handlers are contributed as Routes through an Fx value group, so
// a later story adds an endpoint by providing a constructor.
package httpserver

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"

	"go.uber.org/fx"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/correlation"
)

const RouteGroup = `group:"routes"`

// Route is one entry in the server's mux. Pattern uses net/http's method-aware
// syntax, e.g. "GET /wallets/{walletId}".
type Route struct {
	Pattern string
	Handler http.Handler
}

// AsRoute annotates a constructor returning a Route so it joins the group:
//
//	fx.Provide(httpserver.AsRoute(health.NewLiveRoute))
func AsRoute(constructor any) any {
	return fx.Annotate(constructor, fx.ResultTags(RouteGroup))
}

func NewServeMux(routes []Route) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	for _, route := range routes {
		if route.Pattern == "" || route.Handler == nil {
			return nil, fmt.Errorf("invalid route %q: pattern and handler are required", route.Pattern)
		}
		mux.Handle(route.Pattern, route.Handler)
	}
	return mux, nil
}

// CorrelationHeader is both directions: the id a caller sends is adopted, and
// the one in use is echoed so the caller can quote it (§12).
const CorrelationHeader = "X-Correlation-Id"

func correlated(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, id := correlation.Ensure(r.Context(), r.Header.Get(CorrelationHeader))
		w.Header().Set(CorrelationHeader, id.String())
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func NewServer(lc fx.Lifecycle, cfg config.Config, mux *http.ServeMux, logger *slog.Logger) *http.Server {
	server := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           correlated(mux),
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
	}

	lc.Append(fx.Hook{
		// Listening here rather than in the goroutine makes a taken port a
		// startup failure instead of a log line.
		OnStart: func(ctx context.Context) error {
			listener, err := net.Listen("tcp", server.Addr)
			if err != nil {
				return fmt.Errorf("listen on %s: %w", server.Addr, err)
			}
			// Resolved address, so a configured port of 0 is discoverable.
			// Serve never reads Addr, so writing it here is safe.
			server.Addr = listener.Addr().String()
			logger.Info("http server listening", slog.String("addr", server.Addr))

			go func() {
				if err := server.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
					logger.Error("http server stopped unexpectedly", slog.Any("error", err))
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, cfg.ShutdownTimeout)
			defer cancel()

			if err := server.Shutdown(ctx); err != nil {
				logger.Warn("graceful shutdown deadline exceeded, closing connections",
					slog.Any("error", err))
				return server.Close()
			}
			logger.Info("http server stopped")
			return nil
		},
	})

	return server
}

// The Invoke forces construction: nothing depends on the server.
var Module = fx.Module("httpserver",
	fx.Provide(
		fx.Annotate(NewServeMux, fx.ParamTags(RouteGroup)),
		NewServer,
	),
	fx.Invoke(func(*http.Server) {}),
)
