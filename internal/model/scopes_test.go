package model_test

import (
	"slices"
	"testing"

	"github.com/tabloy/keygate/internal/model"
)

// The consumer payments API is scoped separately for reads and writes so
// a merchant backend can poll orders without being able to create or
// cancel them. Both must be in the closed enumeration, or CreateAPIKey
// rejects the very keys the routes require.
func TestPaymentScopesRegistered(t *testing.T) {
	for _, scope := range []string{model.ScopePaymentsWrite, model.ScopePaymentsRead} {
		if !model.IsValidScope(scope) {
			t.Fatalf("%q is not a valid scope", scope)
		}
	}
	if !slices.Contains(model.AllScopes(), model.ScopePaymentsWrite) ||
		!slices.Contains(model.AllScopes(), model.ScopePaymentsRead) {
		t.Fatalf("AllScopes() = %v, want both payment scopes", model.AllScopes())
	}
}

func TestPaymentScopeNames(t *testing.T) {
	if model.ScopePaymentsWrite != "payments:write" {
		t.Fatalf("ScopePaymentsWrite = %q", model.ScopePaymentsWrite)
	}
	if model.ScopePaymentsRead != "payments:read" {
		t.Fatalf("ScopePaymentsRead = %q", model.ScopePaymentsRead)
	}
}
