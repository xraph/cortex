package fabriqbrain

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/plugin"
)

type fakeRememberer struct {
	reqs []agent.RememberRequest
	err  error
}

func (f *fakeRememberer) Remember(_ context.Context, req agent.RememberRequest) (command.Result, error) {
	f.reqs = append(f.reqs, req)
	return command.Result{}, f.err
}

func TestPlugin_ImplementsExtensionAndHooks(_ *testing.T) {
	var _ plugin.Extension = (*Plugin)(nil)
	var _ plugin.RunStarted = (*Plugin)(nil)
	var _ plugin.RunCompleted = (*Plugin)(nil)
	var _ plugin.RunFailed = (*Plugin)(nil)
}

func TestPlugin_WritesMemoryOnRunCompleted(t *testing.T) {
	rem := &fakeRememberer{}
	p := NewPlugin(rem, WithMemoryEntity("agent_memory"))

	agentID := id.AgentID{}
	runID := id.AgentRunID{}
	ctx := context.Background()

	if err := p.OnRunStarted(ctx, agentID, runID, "what is fabriq?"); err != nil {
		t.Fatalf("OnRunStarted: %v", err)
	}
	if err := p.OnRunCompleted(ctx, agentID, runID, "a data fabric", 2*time.Second); err != nil {
		t.Fatalf("OnRunCompleted: %v", err)
	}

	if len(rem.reqs) != 1 {
		t.Fatalf("got %d Remember calls, want 1", len(rem.reqs))
	}
	req := rem.reqs[0]
	if req.Entity != "agent_memory" || req.Op != "create" {
		t.Fatalf("req = %+v, want entity=agent_memory op=create", req)
	}
	var payload map[string]any
	if err := json.Unmarshal(req.Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	content, _ := payload["content"].(string)
	if !strings.Contains(content, "what is fabriq?") || !strings.Contains(content, "a data fabric") {
		t.Fatalf("content = %q, want it to contain the input and output", content)
	}
	meta, ok := payload["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta missing or not an object: %v", payload["meta"])
	}
	if meta["input"] != "what is fabriq?" || meta["output"] != "a data fabric" {
		t.Fatalf("meta = %v", meta)
	}
}

func TestPlugin_SwallowsWriteErrors(t *testing.T) {
	rem := &fakeRememberer{err: context.DeadlineExceeded}
	p := NewPlugin(rem)
	// A write failure must NOT propagate as a hook error (the registry logs it,
	// but returning nil keeps run completion clean and matches our intent).
	if err := p.OnRunCompleted(context.Background(), id.AgentID{}, id.AgentRunID{}, "out", 0); err != nil {
		t.Fatalf("OnRunCompleted returned error %v, want nil", err)
	}
}

func TestPlugin_SkipsEmptyContent(t *testing.T) {
	rem := &fakeRememberer{}
	p := NewPlugin(rem)
	// OnRunCompleted with no prior OnRunStarted (inflight miss → input "") and
	// empty output → content is empty → no write attempted.
	if err := p.OnRunCompleted(context.Background(), id.AgentID{}, id.AgentRunID{}, "", 0); err != nil {
		t.Fatalf("OnRunCompleted: %v", err)
	}
	if len(rem.reqs) != 0 {
		t.Fatalf("expected no Remember call for empty content, got %d", len(rem.reqs))
	}
}

func TestPlugin_OnRunFailedCleansUpAndRecords(t *testing.T) {
	rem := &fakeRememberer{}
	p := NewPlugin(rem)
	runID := id.AgentRunID{}
	ctx := context.Background()
	if err := p.OnRunStarted(ctx, id.AgentID{}, runID, "do a thing"); err != nil {
		t.Fatalf("OnRunStarted: %v", err)
	}
	if err := p.OnRunFailed(ctx, id.AgentID{}, runID, context.DeadlineExceeded); err != nil {
		t.Fatalf("OnRunFailed: %v", err)
	}
	if _, ok := p.inflight.Load(runID.String()); ok {
		t.Fatalf("inflight entry should be deleted after OnRunFailed")
	}
	if len(rem.reqs) != 1 {
		t.Fatalf("got %d Remember calls, want 1", len(rem.reqs))
	}
	var payload map[string]any
	if err := json.Unmarshal(rem.reqs[0].Payload, &payload); err != nil {
		t.Fatalf("payload not JSON: %v", err)
	}
	meta, ok := payload["meta"].(map[string]any)
	if !ok {
		t.Fatalf("meta missing: %v", payload["meta"])
	}
	if meta["failed"] != true || meta["error"] != context.DeadlineExceeded.Error() {
		t.Fatalf("meta = %v", meta)
	}
}
