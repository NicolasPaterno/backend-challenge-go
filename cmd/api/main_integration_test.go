//go:build integration

package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
)

func TestAppStartStop(t *testing.T) {
	t.Setenv("DATABASE_URL", testsupport.PostgresMigrated(t))
	t.Setenv("HTTP_ADDR", "127.0.0.1:0") // let the kernel pick a free port
	t.Setenv("LOG_LEVEL", "warn")
	testsupport.KeycloakEnv(t)

	var server *http.Server
	fxApp := fxtest.New(t, options(), fx.Populate(&server))
	fxApp.RequireStart()

	endpoint := "http://" + server.Addr + "/health/live"

	resp, err := http.Get(endpoint)
	if err != nil {
		t.Fatalf("GET %s: %v", endpoint, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /health/live status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	var body struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want %q", body.Status, "ok")
	}

	fxApp.RequireStop()

	if _, err := http.Get(endpoint); err == nil {
		t.Error("GET /health/live succeeded after stop, want the listener closed")
	}
}

func TestStartFailsWhenPostgresIsUnreachable(t *testing.T) {
	// Port 1 is reserved and never listening.
	t.Setenv("DATABASE_URL", "postgres://wagering:wagering@127.0.0.1:1/wagering?sslmode=disable")
	t.Setenv("HTTP_ADDR", "127.0.0.1:0")
	t.Setenv("STARTUP_TIMEOUT", "5s")
	t.Setenv("LOG_LEVEL", "error")
	testsupport.KeycloakEnv(t)

	fxApp := fx.New(options(), fx.NopLogger, fx.StartTimeout(20*time.Second))
	if err := fxApp.Err(); err != nil {
		t.Fatalf("fx.New() = %v, want the graph to build", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := fxApp.Start(ctx); err == nil {
		_ = fxApp.Stop(ctx)
		t.Fatal("Start() = nil, want a connection error")
	}
}

func TestPortInUseFailsStartup(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer listener.Close()

	t.Setenv("DATABASE_URL", testsupport.PostgresMigrated(t))
	t.Setenv("HTTP_ADDR", listener.Addr().String())
	t.Setenv("LOG_LEVEL", "error")
	testsupport.KeycloakEnv(t)

	fxApp := fx.New(options(), fx.NopLogger)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if err := fxApp.Start(ctx); err == nil {
		_ = fxApp.Stop(ctx)
		t.Fatal("Start() = nil, want an address-in-use error")
	}
}
