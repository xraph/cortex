package engine_test

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/store/scopespy"
)

// TestStreamAgent_FailurePersistsWithUncancelledContext is the failure
// sibling of TestStreamAgent_CancelPersistsWithUncancelledContext: a
// failure can also arrive as a plain error from stream.Next on a ctx
// that's already been cancelled, rather than through the loop's
// ctx.Done() select case (which was already fixed). Before failRun used
// context.WithoutCancel for its terminal write, that UpdateRun call would
// fail before it started and the run would stay "running" forever
// instead of recording StateFailed.
//
// FailingStreamLLM's stream signals Started the instant its Next call is
// entered — proving the loop took the select's default branch, not the
// ctx.Done() case — then blocks until the test cancels ctx and returns a
// plain error unrelated to ctx.Err(), forcing the loop into the err !=
// nil branch that calls failRun with an already-cancelled context.
func TestStreamAgent_FailurePersistsWithUncancelledContext(t *testing.T) {
	spy := scopespy.New()
	client, stream := scopespy.FailingStreamLLM()
	e, err := engine.New(engine.WithStore(spy), engine.WithLLM(client))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	want := cortex.Scope{Levels: []cortex.Level{
		{Key: "workspace", Value: "ws_x"},
		{Key: "project", Value: "proj_y"},
	}}
	baseCtx := cortex.WithScope(context.Background(), want)
	ctx, cancel := context.WithCancel(baseCtx)
	defer cancel()

	events := make(chan engine.StreamEvent, 64)
	if err := e.StreamAgent(ctx, "assistant", "hello", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}

	// Wait for the stream double to actually be inside Next before
	// cancelling — otherwise cancellation could land before the loop's
	// select even runs once, which would route through the already-fixed
	// ctx.Done() branch instead of the one this test targets.
	<-stream.Started
	cancel()

	var drained int
	for range events {
		drained++
	}
	if drained == 0 {
		t.Fatal("no stream events received; StreamAgent may not have run")
	}

	var sawUpdate bool
	for _, c := range spy.Calls() {
		if c.Method != "UpdateRun" {
			continue
		}
		sawUpdate = true
		if c.Scope.IsZero() {
			t.Error("UpdateRun on failure received a zero scope")
		}
		if c.Scope.Canonical() != want.Canonical() {
			t.Errorf("UpdateRun on failure got scope %q, want %q", c.Scope.Canonical(), want.Canonical())
		}
		if c.CtxErr != nil {
			t.Errorf("UpdateRun on failure got ctx.Err() = %v, want nil (the write must use context.WithoutCancel)", c.CtxErr)
		}
	}
	if !sawUpdate {
		t.Fatal("UpdateRun was never recorded on failure; the failRun path did not run")
	}
}
