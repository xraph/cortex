package engine

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

func TestApprovalAtFinalStepRecordsDecisionWithoutRedispatch(t *testing.T) {
	spy := scopespy.New()
	def := gatedTool()
	var calls atomic.Int32
	recorder := &checkpointEventRecorder{}
	e, err := New(WithStore(spy), WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "last-call", Name: def.Name, Arguments: `{}`}}}), WithTool(def, func(context.Context, cortex.Invocation) (string, error) {
		calls.Add(1)
		return "applied", nil
	}), WithToolAuthorizer(escalatingAuthorizer{tool: def.Name}), WithExtension(recorder))
	if err != nil {
		t.Fatal(err)
	}
	ctx := cortex.WithScope(context.Background(), approvalScope())
	paused, err := e.RunAgent(ctx, "assistant", "apply once", &RunOverrides{MaxSteps: 1})
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("pause: %v %v", paused, err)
	}
	cp := spy.Checkpoints()[0]
	decision := checkpoint.Decision{Approved: true, DecidedBy: "operator"}
	if err = e.ResolveCheckpoint(ctx, cp.ID, decision); !errors.Is(err, cortex.ErrMaxStepsReached) {
		t.Fatalf("expected incomplete execution outcome: %v", err)
	}
	saved, err := spy.GetRun(ctx, paused.ID)
	if err != nil || saved.State != run.StateIncomplete || saved.StopReason != run.StopStepLimit {
		t.Fatalf("run: %v %v", saved, err)
	}
	decided, err := spy.GetCheckpoint(ctx, cp.ID)
	if err != nil || decided.State == checkpointStatePending || decided.Decision == nil || !decided.Decision.Approved {
		t.Fatalf("approval was applied but not recorded: %v %v", decided, err)
	}
	if len(recorder.resolvedEvents()) != 1 || calls.Load() != 1 {
		t.Fatalf("events=%v calls=%d", recorder.resolvedEvents(), calls.Load())
	}
	if err = e.ResolveCheckpoint(ctx, cp.ID, decision); !errors.Is(err, cortex.ErrInvalidState) || calls.Load() != 1 {
		t.Fatalf("repeat decision: %v calls=%d", err, calls.Load())
	}
}
