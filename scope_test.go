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

// TestWithScope_RejectsEmptyKeyOrValue pins the phase's core hazard at the
// type level: a Level with an empty Key or Value would otherwise flatten
// to a "key=" predicate that matches every row sharing that partial key,
// i.e. a shared bucket. WithScope must refuse to attach it rather than
// storing a scope that looks populated and isn't discriminating.
func TestWithScope_RejectsEmptyKeyOrValue(t *testing.T) {
	tests := []struct {
		name   string
		levels []cortex.Level
	}{
		{"empty key", []cortex.Level{{Key: "", Value: "ws_x"}}},
		{"empty value", []cortex.Level{{Key: "workspace", Value: ""}}},
		{
			"empty value in a later level",
			[]cortex.Level{
				{Key: "workspace", Value: "ws_x"},
				{Key: "project", Value: ""},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: tt.levels})
			if got := cortex.ScopeFromContext(ctx); !got.IsZero() {
				t.Errorf("ScopeFromContext after invalid WithScope = %+v, want zero scope", got)
			}
		})
	}
}

// TestWithScope_RejectsTooManyLevels pins the second hole: levels past
// the indexed columns are written to scope_extra but never read back as a
// predicate, even in exact mode, so a caller-supplied value would be
// silently accepted and then ignored by every query.
func TestWithScope_RejectsTooManyLevels(t *testing.T) {
	tooDeep := cortex.Scope{Levels: []cortex.Level{
		{Key: "workspace", Value: "ws_x"},
		{Key: "project", Value: "proj_y"},
		{Key: "environment", Value: "prod"},
		{Key: "region", Value: "us_east"},
	}}
	ctx := cortex.WithScope(context.Background(), tooDeep)
	if got := cortex.ScopeFromContext(ctx); !got.IsZero() {
		t.Errorf("ScopeFromContext after too-deep WithScope = %+v, want zero scope", got)
	}
}

// TestWithScope_AcceptsExactlyMaxLevels proves the boundary itself isn't
// rejected: three levels all land in indexed columns, so this must still
// attach normally.
func TestWithScope_AcceptsExactlyMaxLevels(t *testing.T) {
	want := cortex.Scope{Levels: []cortex.Level{
		{Key: "workspace", Value: "ws_x"},
		{Key: "project", Value: "proj_y"},
		{Key: "environment", Value: "prod"},
	}}
	ctx := cortex.WithScope(context.Background(), want)
	got := cortex.ScopeFromContext(ctx)
	if got.Canonical() != want.Canonical() {
		t.Errorf("ScopeFromContext = %q, want %q", got.Canonical(), want.Canonical())
	}
}
