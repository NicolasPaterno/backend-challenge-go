package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestPreflightIsAnsweredWithoutReachingTheHandler(t *testing.T) {
	handler := crossOrigin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))

	req := httptest.NewRequest(http.MethodOptions, "/wallets", nil)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight status = %d, want %d", rec.Code, http.StatusNoContent)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/wallets/x", nil))

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET status = %d, want the handler's %d", rec.Code, http.StatusUnauthorized)
	}
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", got)
	}
}
