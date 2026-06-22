package fabriqbrain

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
)

type fakeRememberer struct {
	reqs []agent.RememberRequest
	err  error
}

func (f *fakeRememberer) Remember(_ context.Context, req agent.RememberRequest) (command.Result, error) {
	f.reqs = append(f.reqs, req)
	return command.Result{}, f.err
}

func TestPlugin_ImplementsExtensionAndHooks(t *testing.T) {
	var _ plugin.Extension = (*Plugin)(nil)
	var _ plugin.RunStarted = (*Plugin)(nil)
	var _ plugin.RunCompleted = (*Plugin)(nil)
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
	if payload["input"] != "what is fabriq?" || payload["output"] != "a data fabric" {
		t.Fatalf("payload = %v", payload)
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
