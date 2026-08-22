package api

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
)

func TestScopeFromTenant_EmptyTenantYieldsNoScope(t *testing.T) {
	ctx := scopeFromTenant(context.Background())
	if !cortex.ScopeFromContext(ctx).IsZero() {
		t.Fatal("an absent tenant must not manufacture a scope")
	}
}

func TestScopeFromTenant_RealTenantBecomesOneLevel(t *testing.T) {
	ctx := scopeFromTenant(cortex.WithTenant(context.Background(), "acme"))
	got := cortex.ScopeFromContext(ctx)
	if got.Canonical() != "tenant=acme" {
		t.Fatalf("scope = %q, want tenant=acme", got.Canonical())
	}
}
