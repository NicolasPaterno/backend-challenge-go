package main

import (
	"testing"

	"go.uber.org/fx"
)

// ValidateApp walks the graph without running constructors, so this stays a
// unit test; the start/stop cycle lives in main_integration_test.go.
func TestOptionsGraphIsSatisfiable(t *testing.T) {
	if err := fx.ValidateApp(options()); err != nil {
		t.Fatalf("fx.ValidateApp(options()) = %v, want nil", err)
	}
}

func TestNewFailsOnInvalidConfiguration(t *testing.T) {
	t.Setenv("DATABASE_URL", "")

	if err := fx.New(options(), fx.NopLogger).Err(); err == nil {
		t.Fatal("fx.New(options()).Err() = nil, want a configuration error")
	}
}
