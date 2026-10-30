package contract

import (
	"context"
	"testing"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

func TestAssuranceCapabilitiesAndLegacyRunProjection(t *testing.T) {
	d, s, st := testService(t)
	p := principal("assurance")
	raw, err := dispatch(t, d, p, "runtime.detail", struct{}{})
	if err != nil {
		t.Fatal(err)
	}
	runtime := decode[struct {
		Assurance engine.RuntimeCapabilities `json:"assurance"`
	}](t, raw)
	if !runtime.Assurance.PreparedExecution || runtime.Assurance.ContextualVerification || runtime.Assurance.AtomicSourceSnapshot {
		t.Fatalf("inaccurate capabilities: %+v", runtime.Assurance)
	}
	ctx, err := s.context(context.Background(), p, "manage")
	if err != nil {
		t.Fatal(err)
	}
	ag := &agent.Config{ID: id.NewAgentID(), Name: "legacy", Enabled: true}
	if err = s.deps.Engine.CreateAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	for _, state := range []run.State{run.StateCompleted, run.StateIncomplete} {
		r := &run.Run{ID: id.NewAgentRunID(), AgentID: ag.ID, State: state}
		if state == run.StateIncomplete {
			r.StopReason = run.StopStepLimit
		}
		if err = st.CreateRun(ctx, r); err != nil {
			t.Fatal(err)
		}
		raw, err = dispatch(t, d, p, "runs.detail", IDInput{ID: r.ID.String()})
		if err != nil {
			t.Fatal(err)
		}
		out := decode[RunDetail](t, raw)
		if out.Run.VerificationStatus != "unchecked" || out.Run.State != state || out.Run.StopReason != r.StopReason {
			t.Fatalf("incorrect outcome: %+v", out.Run)
		}
		if _, err = dispatch(t, d, principal("foreign"), "runs.detail", IDInput{ID: r.ID.String()}); err == nil {
			t.Fatal("foreign principal read outcome")
		}
	}
}

func TestRequiredSuspensionDetailHidesPrivateCalls(t *testing.T) {
	d, s, st := testService(t)
	p := principal("private-handoff")
	ctx, err := s.context(context.Background(), p, "manage")
	if err != nil {
		t.Fatal(err)
	}
	ag := &agent.Config{ID: id.NewAgentID(), Name: "private", Enabled: true}
	if err = s.deps.Engine.CreateAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StatePaused, Provenance: run.ExecutionProvenance{VerificationRequired: true}}
	if err = st.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	susp := &suspension.Suspension{ID: id.NewSuspensionID(), RunID: r.ID, Reason: suspension.ReasonExternalTool, Pending: []suspension.PendingCall{{ID: "SECRET-ID", Name: "probe", Arguments: "SECRET arguments"}}}
	if err = st.CreateSuspension(ctx, susp); err != nil {
		t.Fatal(err)
	}
	raw, err := dispatch(t, d, p, "runs.detail", IDInput{ID: r.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[RunDetail](t, raw)
	if out.Suspension == nil || len(out.Suspension.Pending) != 0 {
		t.Fatalf("private pending calls exposed: %+v", out.Suspension)
	}
}

// A ready run still has the pause it is about to continue from, and the
// detail shows its pending calls as it does for a paused run.
func TestReadyRunDetailShowsItsPause(t *testing.T) {
	d, s, st := testService(t)
	p := principal("ready-detail")
	ctx, err := s.context(context.Background(), p, "manage")
	if err != nil {
		t.Fatal(err)
	}
	ag := &agent.Config{ID: id.NewAgentID(), Name: "ready", Enabled: true}
	if err = s.deps.Engine.CreateAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StateReady}
	if err = st.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	susp := &suspension.Suspension{ID: id.NewSuspensionID(), RunID: r.ID, Reason: suspension.ReasonExternalTool, Pending: []suspension.PendingCall{{ID: "call-1", Name: "probe", Arguments: "{}"}}}
	if err = st.CreateSuspension(ctx, susp); err != nil {
		t.Fatal(err)
	}
	raw, err := dispatch(t, d, p, "runs.detail", IDInput{ID: r.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	out := decode[RunDetail](t, raw)
	if out.Suspension == nil || len(out.Suspension.Pending) != 1 || out.Suspension.Pending[0].ID != "call-1" {
		t.Fatalf("ready run detail suspension = %+v, want its pending call", out.Suspension)
	}
}
