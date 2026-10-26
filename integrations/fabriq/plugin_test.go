package fabriqbrain

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/command"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	cagent "github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/sqlite"
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
	if meta["evidence_class"] != "observation" || meta["input"] != "what is fabriq?" || meta["output"] != "a data fabric" {
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
	if meta["evidence_class"] != "failed_approach" || meta["failed"] != true || meta["error"] != context.DeadlineExceeded.Error() {
		t.Fatalf("meta = %v", meta)
	}
}

// A trial's output must not become ordinary recall material, including failures
// and terminal callbacks whose host no longer carries the start context.
func TestTrialRunsDoNotWriteOrdinaryMemory(t *testing.T) {
	for _, purpose := range []run.ExecutionPurpose{run.PurposeNormal, run.PurposeEvaluation, run.PurposeOptimizationTrial, run.PurposeOptimizationController} {
		for _, failed := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/failed=%t", purpose, failed), func(t *testing.T) {
				ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "learning"}}})
				driver := sqlitedriver.New()
				if err := driver.Open(ctx, filepath.Join(t.TempDir(), "runs.db")); err != nil {
					t.Fatal(err)
				}
				db, err := grove.Open(driver)
				if err != nil {
					t.Fatal(err)
				}
				storage := sqlite.New(db)
				defer storage.Close()
				if err = storage.Migrate(ctx); err != nil {
					t.Fatal(err)
				}
				config := &cagent.Config{ID: id.NewAgentID(), Name: "worker", Model: "fixture"}
				if err = storage.Create(ctx, config); err != nil {
					t.Fatal(err)
				}
				rem := &fakeRememberer{}
				mapped := 0
				p := NewPlugin(rem, WithTenantMapper(func(ctx context.Context) context.Context { mapped++; return ctx }))
				model := &provenanceModel{failed: failed}
				eng, err := engine.New(engine.WithStore(storage), engine.WithLLM(model), engine.WithExtension(p))
				if err != nil {
					t.Fatal(err)
				}
				prepared, err := eng.PrepareAgent(ctx, config.ID, nil)
				if err != nil {
					t.Fatal(err)
				}
				_, err = eng.RunPrepared(ctx, prepared, "private trial input", engine.PreparedRunOptions{Purpose: purpose})
				if (err != nil) != failed {
					t.Fatalf("run failure=%t: %v", failed, err)
				}
				want := 0
				if purpose == run.PurposeNormal {
					want = 1
				}
				if len(rem.reqs) != want || mapped != want {
					t.Fatalf("purpose %s wrote %d ordinary memories and mapped %d scopes, want %d", purpose, len(rem.reqs), mapped, want)
				}
				runID := id.NewAgentRunID()
				if err = p.OnRunStarted(model.ctx, config.ID, runID, "private restarted input"); err != nil {
					t.Fatal(err)
				}
				if failed {
					err = p.OnRunFailed(context.Background(), config.ID, runID, errors.New("private failure"))
				} else {
					err = p.OnRunCompleted(context.Background(), config.ID, runID, "private output", time.Second)
				}
				if err != nil {
					t.Fatal(err)
				}
				if len(rem.reqs) != 2*want {
					t.Fatalf("terminal context loss wrote %d memories, want %d", len(rem.reqs), 2*want)
				}
				if _, found := p.inflight.Load(runID.String()); found {
					t.Fatal("terminal callback retained input")
				}
			})
		}
	}
}

type provenanceModel struct {
	ctx    context.Context
	failed bool
}

func (p *provenanceModel) Complete(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
	p.ctx = ctx
	if p.failed {
		return nil, errors.New("fixture failure")
	}
	return &llm.Response{Content: "private trial output", Model: "fixture", FinishReason: "stop"}, nil
}
func (*provenanceModel) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("unused stream")
}
