package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/store/scopespy"
)

// Both tests call RunAgent with an explicit appID because Task 9 has not
// yet removed that parameter. When Task 9 lands, drop the "app1" argument
// from both calls.

func TestRun_RejectsZeroScope(t *testing.T) {
	spy := scopespy.New()
	e, err := engine.New(engine.WithStore(spy))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	_, err = e.RunAgent(context.Background(), "app1", "assistant", "hello", nil)
	if !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("err = %v, want ErrNoScope", err)
	}
	if spy.CallCount() != 0 {
		t.Errorf("store was called %d times on a zero scope; want 0", spy.CallCount())
	}
}

// TestRun_EveryStoreCallCarriesScope is the regression guard for the
// cross-tenant conversation bleed. The react loop used to pass "" as the
// tenant on all four conversation calls, so every tenant shared one
// history bucket. The spy fails the test if any recorded call arrives
// with a zero scope, which is what a future forgotten call site looks like.
func TestRun_EveryStoreCallCarriesScope(t *testing.T) {
	spy := scopespy.New()
	e, err := engine.New(engine.WithStore(spy), engine.WithLLM(scopespy.StaticLLM("done")))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	want := cortex.Scope{Levels: []cortex.Level{
		{Key: "workspace", Value: "ws_x"},
		{Key: "project", Value: "proj_y"},
	}}
	ctx := cortex.WithScope(context.Background(), want)

	if _, err := e.RunAgent(ctx, "app1", "assistant", "hello", nil); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	if spy.CallCount() == 0 {
		t.Fatal("spy recorded no store calls; the test proves nothing")
	}
	for _, c := range spy.Calls() {
		if c.Scope.IsZero() {
			t.Errorf("%s received a zero scope", c.Method)
		}
		if c.Scope.Canonical() != want.Canonical() {
			t.Errorf("%s got scope %q, want %q", c.Method, c.Scope.Canonical(), want.Canonical())
		}
	}
}
