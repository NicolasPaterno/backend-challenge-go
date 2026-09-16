package httpserver_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/health"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/httpserver"
)

func TestNewServeMuxServesContributedRoutes(t *testing.T) {
	mux, err := httpserver.NewServeMux([]httpserver.Route{health.NewLiveRoute()})
	if err != nil {
		t.Fatalf("NewServeMux() error = %v, want nil", err)
	}

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/health/live", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("GET /health/live status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if body := strings.TrimSpace(rec.Body.String()); body != `{"status":"ok"}` {
		t.Errorf("body = %s, want {\"status\":\"ok\"}", body)
	}
}

// A half-built route would otherwise panic inside net/http at registration.
func TestNewServeMuxRejectsIncompleteRoute(t *testing.T) {
	for name, route := range map[string]httpserver.Route{
		"no pattern": {Handler: http.NotFoundHandler()},
		"no handler": {Pattern: "GET /x"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := httpserver.NewServeMux([]httpserver.Route{route}); err == nil {
				t.Fatal("NewServeMux() error = nil, want an error")
			}
		})
	}
}
