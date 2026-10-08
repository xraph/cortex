package contract

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	dc "github.com/xraph/forge/extensions/dashboard/contract"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
)

type testLLM struct{}

func (testLLM) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	return &llm.Response{Content: "Verified response", FinishReason: "stop"}, nil
}
func (testLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return &testStream{}, nil
}

type testStream struct{ index int }

func (s *testStream) Next(context.Context) (*llm.Chunk, error) {
	s.index++
	switch s.index {
	case 1:
		return &llm.Chunk{Content: "Verified "}, nil
	case 2:
		return &llm.Chunk{Content: "response", FinishReason: "stop"}, nil
	default:
		return nil, io.EOF
	}
}
func (*testStream) Close() error      { return nil }
func (*testStream) Usage() *llm.Usage { return &llm.Usage{TotalTokens: 2} }
func decode[T any](t *testing.T, raw json.RawMessage) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}
func TestExecutionSessionStreamAndForeignIDs(t *testing.T) {
	d, s, _ := testService(t, engine.WithLLM(testLLM{}))
	a, b := principal("a"), principal("b")
	ctx, err := s.context(context.Background(), a, "manage")
	if err != nil {
		t.Fatal(err)
	}
	ag := &agent.Config{ID: id.NewAgentID(), Name: "runner", Enabled: true, Sections: []prompt.Section{{ID: "role", Body: "Read the evidence", Order: 1}}}
	if err = s.deps.Engine.CreateAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	raw, err := dispatch(t, d, a, "sessions.create", SessionCreate{AgentID: ag.ID.String(), Title: "Persistent thread"})
	if err != nil {
		t.Fatal(err)
	}
	ss := decode[struct {
		ID string `json:"id"`
	}](t, raw)
	for _, intent := range []string{"prompt.preview", "agents.clone", "runs.start"} {
		_, err = dispatch(t, d, b, intent, ExecuteInput{ID: ag.ID.String(), Input: "hello"})
		if intent == "agents.clone" {
			_, err = dispatch(t, d, b, intent, CloneInput{ID: ag.ID.String(), Name: "copy"})
		}
		if !errors.Is(err, dc.ErrNotFound) {
			t.Fatalf("%s foreign: %v", intent, err)
		}
	}
	raw, err = dispatch(t, d, a, "runs.start", ExecuteInput{ID: ag.ID.String(), Input: "hello", SessionID: ss.ID})
	if err != nil {
		t.Fatal(err)
	}
	started := decode[struct {
		ID string `json:"id"`
	}](t, raw)
	var feed StreamOutput
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err = dispatch(t, d, a, "runs.events", StreamInput{ID: started.ID})
		if err != nil {
			t.Fatal(err)
		}
		feed = decode[StreamOutput](t, raw)
		if feed.Done {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !feed.Done || !feed.Available || len(feed.Events) < 3 {
		t.Fatalf("stream %+v", feed)
	}
	token := ""
	for _, ev := range feed.Events {
		if ev.Type == engine.EventToken {
			if chunk, ok := ev.Data["content"].(string); ok {
				token += chunk
			}
		}
	}
	if token != "Verified response" {
		t.Fatalf("stream tokens %q", token)
	}
	raw, err = dispatch(t, d, a, "runs.detail", IDInput{started.ID})
	if err != nil {
		t.Fatal(err)
	}
	detail := decode[RunDetail](t, raw)
	if detail.Run.State != run.StateCompleted || detail.Run.SessionID.String() != ss.ID || len(detail.Steps) != 1 {
		t.Fatalf("run %+v", detail)
	}
	raw, err = dispatch(t, d, a, "memory.detail", MemoryInput{ID: ss.ID})
	if err != nil {
		t.Fatal(err)
	}
	history := decode[MemoryOutput](t, raw)
	if len(history.Messages) != 2 || !history.Complete {
		t.Fatalf("history %+v", history)
	}
	for _, intent := range []string{"runs.detail", "runs.events", "runs.cancel", "runs.resume"} {
		_, err = dispatch(t, d, b, intent, IDInput{started.ID})
		if !errors.Is(err, dc.ErrNotFound) {
			t.Fatalf("%s foreign %v", intent, err)
		}
	}
	for _, intent := range []string{"memory.detail", "memory.clear", "sessions.update", "sessions.delete"} {
		_, err = dispatch(t, d, b, intent, IDInput{ss.ID})
		if !errors.Is(err, dc.ErrNotFound) {
			t.Fatalf("%s foreign %v", intent, err)
		}
	}
	if _, err = dispatch(t, d, a, "memory.clear", IDInput{ss.ID}); err != nil {
		t.Fatal(err)
	}
	raw, err = dispatch(t, d, a, "memory.detail", MemoryInput{ID: ss.ID})
	if err != nil {
		t.Fatal(err)
	}
	history = decode[MemoryOutput](t, raw)
	if len(history.Messages) != 0 {
		t.Fatal("clear retained messages")
	}
}
func TestNoLLMAndHostOverlayPermission(t *testing.T) {
	d, s, _ := testService(t)
	a := principal("a")
	ctx, _ := s.context(context.Background(), a, "manage")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "runner", Enabled: true}
	if err := s.deps.Engine.CreateAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	for _, intent := range []string{"runs.start", "runs.execute"} {
		if _, err := dispatch(t, d, a, intent, ExecuteInput{ID: ag.ID.String(), Input: "hello"}); !errors.Is(err, dc.ErrUnavailable) {
			t.Fatalf("echo fallback: %v", err)
		}
	}
	if _, err := dispatch(t, d, a, "overlays.save", OverlaySaveInput{AgentID: ag.ID.String()}); !errors.Is(err, dc.ErrPermissionDenied) {
		t.Fatal(err)
	}
	a.User.Scopes = append(a.User.Scopes, "cortex.overlay")
	if _, err := dispatch(t, d, a, "overlays.save", OverlaySaveInput{AgentID: ag.ID.String(), Patches: []prompt.Patch{{ID: "review", Body: "Check sources", Mode: prompt.PatchAppend}}}); err != nil {
		t.Fatal(err)
	}
	raw, err := dispatch(t, d, a, "prompt.preview", ExecuteInput{ID: ag.ID.String()})
	if err != nil {
		t.Fatal(err)
	}
	if decode[map[string]string](t, raw)["prompt"] != "Check sources" {
		t.Fatalf("overlay preview %s", raw)
	}
}
func TestCheckpointDecisionUsesPrincipalAndRefusesRepeat(t *testing.T) {
	d, s, st := testService(t)
	a, b := principal("a"), principal("b")
	ctx, _ := s.context(context.Background(), a, "manage")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "runner", Enabled: true}
	if err := s.deps.Engine.CreateAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StateRunning}
	if err := st.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	cp := &checkpoint.Checkpoint{ID: id.NewCheckpointID(), RunID: r.ID, AgentID: ag.ID, State: "pending", Reason: "Host review"}
	if err := st.CreateCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	in := DecisionInput{ID: cp.ID.String(), Approved: true, Reason: "Evidence reviewed"}
	if _, err := dispatch(t, d, b, "checkpoints.resolve", in); !errors.Is(err, dc.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := dispatch(t, d, a, "checkpoints.resolve", in); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetCheckpoint(ctx, cp.ID)
	if err != nil || got.Decision == nil || got.Decision.DecidedBy != a.User.Subject {
		t.Fatalf("decision %+v %v", got, err)
	}
	if _, err = dispatch(t, d, a, "checkpoints.resolve", in); !errors.Is(err, dc.ErrConflict) {
		t.Fatalf("repeat: %v", err)
	}
}
func TestAuditFailureAfterMutationReportsAttempt(t *testing.T) {
	_, s, _ := testService(t)
	a := principal("a")
	ctx, _ := s.context(context.Background(), a, "manage")
	calls := 0
	s.deps.Audit = func(context.Context, AuditEvent) error {
		calls++
		if calls == 2 {
			return errors.New("disk full")
		}
		return nil
	}
	ran := false
	err := s.write(ctx, a, "test", "resource", func() error { ran = true; return nil })
	var ce *dc.Error
	if !ran || !errors.As(err, &ce) || ce.Details["command_attempted"] != true {
		t.Fatalf("audit: %v", err)
	}
	if _, err = s.deps.Engine.Store().GetRun(context.Background(), id.NewAgentRunID()); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatal(err)
	}
}

type cancelLLM struct {
	testLLM
	entered chan struct{}
}

func (c cancelLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return &cancelProviderStream{entered: c.entered}, nil
}

type cancelProviderStream struct{ entered chan struct{} }

func (s *cancelProviderStream) Next(ctx context.Context) (*llm.Chunk, error) {
	close(s.entered)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (*cancelProviderStream) Close() error      { return nil }
func (*cancelProviderStream) Usage() *llm.Usage { return &llm.Usage{} }
func TestLiveCancelInsideProviderRetainsCancelledState(t *testing.T) {
	entered := make(chan struct{})
	d, s, _ := testService(t, engine.WithLLM(cancelLLM{entered: entered}))
	p := principal("a")
	ctx, _ := s.context(context.Background(), p, "manage")
	ag := &agent.Config{ID: id.NewAgentID(), Name: "cancel-test", Enabled: true}
	if err := s.deps.Engine.CreateAgent(ctx, ag); err != nil {
		t.Fatal(err)
	}
	raw, err := dispatch(t, d, p, "runs.start", ExecuteInput{ID: ag.ID.String(), Input: "wait"})
	if err != nil {
		t.Fatal(err)
	}
	started := decode[IDInput](t, raw)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("provider never entered Next")
	}
	if _, cancelErr := dispatch(t, d, p, "runs.cancel", started); cancelErr != nil {
		t.Fatal(cancelErr)
	}
	deadline := time.Now().Add(5 * time.Second)
	done := false
	for time.Now().Before(deadline) {
		raw, err = dispatch(t, d, p, "runs.events", StreamInput{ID: started.ID})
		if err != nil {
			t.Fatal(err)
		}
		if decode[StreamOutput](t, raw).Done {
			done = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !done {
		t.Fatal("cancelled stream did not close")
	}
	raw, err = dispatch(t, d, p, "runs.detail", started)
	if err != nil {
		t.Fatal(err)
	}
	result := decode[RunDetail](t, raw).Run
	if result.State != run.StateCancelled || result.CompletedAt == nil || result.Error != "" {
		t.Fatalf("cancel overwritten: %+v", result)
	}
}
