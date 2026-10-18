package cortex_test

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
)

func TestInvalidScopeNeverRetainsParentAuthority(t *testing.T) {
	parent := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "parent"}}})
	invalid := []cortex.Scope{
		{Levels: []cortex.Level{{Key: "workspace", Value: ""}}},
		{Levels: []cortex.Level{{Key: "workspace", Value: "a"}, {Key: "workspace", Value: "b"}}},
		{Levels: []cortex.Level{{Key: "workspace", Value: "a/project=b"}}},
		{Levels: []cortex.Level{{Key: "workspace", Value: " "}}},
		{Levels: []cortex.Level{{Key: "a", Value: "1"}, {Key: "b", Value: "2"}, {Key: "c", Value: "3"}, {Key: "d", Value: "4"}}},
	}
	for _, scope := range invalid {
		if got := cortex.ScopeFromContext(cortex.WithScope(parent, scope)); !got.IsZero() {
			t.Errorf("invalid scope retained authority: %q", got.Canonical())
		}
	}
}

func TestContextScopeIsDetached(t *testing.T) {
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "original"}}}
	ctx := cortex.WithScope(context.Background(), scope)
	scope.Levels[0].Value = "changed"
	first := cortex.ScopeFromContext(ctx)
	if first.Canonical() != "workspace=original" {
		t.Fatal("caller mutated attached scope")
	}
	first.Levels[0].Value = "changed again"
	if cortex.ScopeFromContext(ctx).Canonical() != "workspace=original" {
		t.Fatal("reader mutated attached scope")
	}
}

func TestCanonicalRejectsUnsupportedIdentity(t *testing.T) {
	for _, value := range []string{"a=1/a=2", "a=1=2", "a=1/b=2/c=3/d=4", "a= "} {
		if _, err := cortex.ParseCanonical(value); err == nil {
			t.Errorf("accepted %q", value)
		}
	}
}
