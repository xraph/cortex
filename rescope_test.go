package cortex_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
)

type stubRescoper struct {
	fn func(appID, tenantID string) (cortex.Scope, error)
}

func (s stubRescoper) Rescope(_ context.Context, appID, tenantID string) (cortex.Scope, error) {
	return s.fn(appID, tenantID)
}

func TestApplyMigrateOptions_NilByDefault(t *testing.T) {
	opts := cortex.ApplyMigrateOptions()
	if opts.Rescoper != nil {
		t.Error("a fresh install must not require a rescoper")
	}
}

func TestApplyMigrateOptions_CarriesRescoper(t *testing.T) {
	r := stubRescoper{fn: func(appID, _ string) (cortex.Scope, error) {
		return cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: appID}}}, nil
	}}
	opts := cortex.ApplyMigrateOptions(cortex.WithRescoper(r))
	if opts.Rescoper == nil {
		t.Fatal("WithRescoper did not attach the rescoper")
	}
	got, err := opts.Rescoper.Rescope(context.Background(), "acme", "")
	if err != nil {
		t.Fatalf("Rescope: %v", err)
	}
	if got.Canonical() != "workspace=acme" {
		t.Errorf("scope = %q, want workspace=acme", got.Canonical())
	}
}

// A rescoper that returns a scope WithScope would reject must not be able to
// smuggle it past validation. An empty-valued level flattens to a "key="
// predicate, which is a shared bucket by another name.
func TestValidateRescopedScope_RejectsEmptyValue(t *testing.T) {
	bad := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: ""}}}
	if err := cortex.ValidateRescopedScope(bad); err == nil {
		t.Error("an empty-valued level must be rejected")
	}
}

func TestValidateRescopedScope_RejectsTooDeep(t *testing.T) {
	bad := cortex.Scope{Levels: []cortex.Level{
		{Key: "a", Value: "1"}, {Key: "b", Value: "2"},
		{Key: "c", Value: "3"}, {Key: "d", Value: "4"},
	}}
	if err := cortex.ValidateRescopedScope(bad); err == nil {
		t.Error("a scope deeper than maxScopeLevels must be rejected")
	}
}

func TestValidateRescopedScope_AcceptsGood(t *testing.T) {
	good := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}
	if err := cortex.ValidateRescopedScope(good); err != nil {
		t.Errorf("valid scope rejected: %v", err)
	}
}

func TestErrNoRescoper_IsDistinct(t *testing.T) {
	if errors.Is(cortex.ErrNoRescoper, cortex.ErrNoScope) {
		t.Error("ErrNoRescoper must not alias ErrNoScope; they mean different things")
	}
}

// A rescoper that returns the zero Scope means the host declined to map
// the row -- there is no level at all, not even an empty-valued one. That
// must be an error, the same as any other scope ValidateRescopedScope
// refuses, or the rescope pass would happily write an unscoped row right
// back to the database it was supposed to be fixing.
func TestValidateRescopedScope_RejectsZeroScope(t *testing.T) {
	if err := cortex.ValidateRescopedScope(cortex.Scope{}); err == nil {
		t.Error("a scope with no levels means the host declined to map the row; that must be an error")
	}
}
