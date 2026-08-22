package cortex_test

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
)

func TestScope_Canonical(t *testing.T) {
	tests := []struct {
		name  string
		scope cortex.Scope
		want  string
	}{
		{"zero", cortex.Scope{}, ""},
		{
			"one level",
			cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}},
			"workspace=ws_x",
		},
		{
			"two levels keep order",
			cortex.Scope{Levels: []cortex.Level{
				{Key: "workspace", Value: "ws_x"},
				{Key: "project", Value: "proj_y"},
			}},
			"workspace=ws_x/project=proj_y",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.scope.Canonical(); got != tt.want {
				t.Errorf("Canonical() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestScope_Get(t *testing.T) {
	s := cortex.Scope{Levels: []cortex.Level{
		{Key: "workspace", Value: "ws_x"},
		{Key: "project", Value: "proj_y"},
	}}

	if v, ok := s.Get("project"); !ok || v != "proj_y" {
		t.Errorf("Get(project) = %q, %v; want proj_y, true", v, ok)
	}
	if _, ok := s.Get("environment"); ok {
		t.Error("Get(environment) returned ok for an absent level")
	}
}

func TestScope_IsZero(t *testing.T) {
	if !(cortex.Scope{}).IsZero() {
		t.Error("empty scope should report IsZero")
	}
	nonEmpty := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}
	if nonEmpty.IsZero() {
		t.Error("populated scope should not report IsZero")
	}
}

func TestScopeFromContext_RoundTrip(t *testing.T) {
	want := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}
	ctx := cortex.WithScope(context.Background(), want)

	got := cortex.ScopeFromContext(ctx)
	if got.Canonical() != want.Canonical() {
		t.Errorf("round trip = %q, want %q", got.Canonical(), want.Canonical())
	}
}

func TestScopeFromContext_AbsentIsZero(t *testing.T) {
	if !cortex.ScopeFromContext(context.Background()).IsZero() {
		t.Error("bare context should yield a zero scope")
	}
}
