package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

type requiredToolAuthority struct {
	denied   bool
	approval bool
}

func (*requiredToolAuthority) Visible(_ context.Context, _ cortex.Subject, defs []llm.Tool) []llm.Tool {
	return defs
}
func (a *requiredToolAuthority) Authorize(context.Context, cortex.Subject, llm.ToolCall) error {
	if a.denied {
		return errors.New("SECRET authority")
	}
	if a.approval {
		return cortex.ErrRequiresApproval
	}
	return nil
}
func (a *requiredToolAuthority) AuthorizeToolHandoff(context.Context, cortex.Subject) error {
	if a.denied {
		return errors.New("SECRET handoff")
	}
	return nil
}

type requiredToolClient struct {
	calls int
	last  *llm.Request
	final bool
	name  string
}

func (c *requiredToolClient) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	c.calls++
	c.last = req
	name := c.name
	if name == "" {
		name = "probe"
	}
	if !c.final && c.calls == 1 {
		return &llm.Response{Content: "SECRET thought", ToolCalls: []llm.ToolCall{{ID: "SECRET-ID", Name: name, Arguments: `{"input":"SECRET argument"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 5}}, nil
	}
	return &llm.Response{Content: "done", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 5}}, nil
}
func (c *requiredToolClient) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	r, err := c.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return &requiredStream{chunks: []llm.Chunk{{Content: r.Content, ToolCalls: r.ToolCalls, FinishReason: r.FinishReason}}}, nil
}

func requiredToolEngine(t *testing.T, f workerFixture, client llm.Client, auth *requiredToolAuthority, extra ...engine.Option) *engine.Engine {
	t.Helper()
	cfg := requiredConfiguration(t, f)
	cfg.HandoffAuthorizer = auth
	opts := make([]engine.Option, 0, 5+len(extra))
	opts = append(opts, engine.WithStore(f.storage), engine.WithLLM(client), engine.WithModelRevision("smart", "model:1"), engine.WithToolRevision("probe", "1"), engine.WithToolAuthorizer(auth), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()), engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(cfg))
	opts = append(opts, extra...)
	e, err := engine.New(opts...)
	require.NoError(t, err)
	return e
}
func requiredToolAgent(t *testing.T, f workerFixture) (*agent.Config, *engine.RunOverrides) {
	t.Helper()
	ag := &agent.Config{ID: id.NewAgentID(), Name: "required-tools", Tools: []string{"probe"}}
	require.NoError(t, f.storage.Create(f.ctx, ag))
	return ag, &engine.RunOverrides{Budget: &execution.Budget{MaxSteps: 4, MaxTokens: 1000, MaxToolCalls: 2, Deadline: time.Now().Add(3 * time.Minute)}}
}

func assertRequiredToolPrivacy(t *testing.T, f workerFixture, ag *agent.Config) {
	t.Helper()
	rows, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	steps, err := f.storage.ListSteps(f.ctx, rows[0].ID)
	require.NoError(t, err)
	for _, step := range steps {
		raw, marshalErr := json.Marshal(step)
		require.NoError(t, marshalErr)
		require.NotContains(t, string(raw), "SECRET")
		calls, callErr := f.storage.ListToolCalls(f.ctx, step.ID)
		require.NoError(t, callErr)
		raw, marshalErr = json.Marshal(calls)
		require.NoError(t, marshalErr)
		require.NotContains(t, string(raw), "SECRET")
	}
	history, err := f.storage.LoadConversation(f.ctx, ag.ID, rows[0].SessionID, 100)
	require.NoError(t, err)
	require.Empty(t, history)
}

func TestRequiredVerificationToolsStayPrivate(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[streaming], func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredToolClient{}
			auth := &requiredToolAuthority{}
			calls := 0
			e := requiredToolEngine(t, f, client, auth, engine.WithTool(llm.Tool{Name: "probe"}, func(_ context.Context, inv cortex.Invocation) (string, error) {
				calls++
				require.Contains(t, inv.Call.Arguments, "SECRET")
				return "SECRET result", nil
			}))
			ag, overrides := requiredToolAgent(t, f)
			if streaming {
				events := make(chan engine.StreamEvent, 20)
				require.NoError(t, e.StreamAgent(f.ctx, ag.Name, "work", overrides, events))
				for event := range events {
					raw, err := json.Marshal(event)
					require.NoError(t, err)
					require.NotContains(t, string(raw), "SECRET")
				}
			} else {
				r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
				require.NoError(t, err)
				require.Equal(t, verification.StatusPassed, r.VerificationStatus)
			}
			require.Equal(t, 1, calls)
			require.Equal(t, 2, client.calls)
			require.Contains(t, client.last.Messages[len(client.last.Messages)-1].Content, "SECRET result")
			assertRequiredToolPrivacy(t, f, ag)
		})
	}
}

func TestRequiredVerificationPrivateHandoffAndResume(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredToolClient{}
	auth := &requiredToolAuthority{}
	e := requiredToolEngine(t, f, client, auth, engine.WithExternalTool(llm.Tool{Name: "probe"}))
	ag, overrides := requiredToolAgent(t, f)
	r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, r.State)
	_, err = e.Resume(f.ctx, r.ID, engine.ResumeInput{})
	require.ErrorIs(t, err, cortex.ErrResultsMismatch)
	require.NotContains(t, err.Error(), "SECRET")
	calls, err := e.PendingVerificationTools(f.ctx, r.ID)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	require.Contains(t, calls[0].Arguments, "SECRET")
	auth.denied = true
	_, err = e.PendingVerificationTools(f.ctx, r.ID)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "SECRET")
	_, err = e.Resume(f.ctx, r.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: calls[0].ID, Content: "SECRET result"}}})
	require.Error(t, err)
	saved, err := f.storage.GetRun(f.ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, saved.State)
	require.Equal(t, r.Revision, saved.Revision)
	auth.denied = false
	require.NoError(t, f.storage.Close())
	f.storage = openSQLiteStoreForTest(t, f.path)
	resumedClient := &requiredToolClient{final: true}
	resumedEngine := requiredToolEngine(t, f, resumedClient, auth, engine.WithExternalTool(llm.Tool{Name: "probe"}))
	r, err = resumedEngine.Resume(f.ctx, r.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: calls[0].ID, Content: "SECRET result"}}})
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, r.VerificationStatus)
	require.Equal(t, 1, client.calls)
	require.Equal(t, 1, resumedClient.calls)
	require.Equal(t, 1, r.Usage.ToolCalls)
	require.Equal(t, 970, r.Budget.MaxTokens)
	assertRequiredToolPrivacy(t, f, ag)
}

type requiredToolHooks struct{ seen []string }

func (*requiredToolHooks) Name() string { return "private-tools" }
func (h *requiredToolHooks) OnToolCalled(_ context.Context, _ id.AgentRunID, name string, args any) error {
	h.seen = append(h.seen, fmt.Sprint(name, args))
	return nil
}
func (h *requiredToolHooks) OnToolCompleted(_ context.Context, _ id.AgentRunID, name, result string, _ time.Duration) error {
	h.seen = append(h.seen, name+result)
	return nil
}
func (h *requiredToolHooks) OnToolFailed(_ context.Context, _ id.AgentRunID, name string, err error) error {
	h.seen = append(h.seen, name+err.Error())
	if cause := errors.Unwrap(err); cause != nil {
		h.seen = append(h.seen, cause.Error())
	}
	return nil
}
func (h *requiredToolHooks) OnToolDenied(_ context.Context, _ id.AgentRunID, name, reason string) error {
	h.seen = append(h.seen, name+reason)
	return nil
}

func TestRequiredVerificationToolHooksRedactFailures(t *testing.T) {
	for _, mode := range []string{"completed", "failed", "denied", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredToolClient{}
			auth := &requiredToolAuthority{denied: mode == "denied"}
			hooks := &requiredToolHooks{}
			dispatched := 0
			if mode == "unknown" {
				client.name = "SECRET unknown tool"
			}
			e := requiredToolEngine(t, f, client, auth, engine.WithExtension(hooks), engine.WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) {
				dispatched++
				if mode == "failed" {
					return "SECRET result", errors.New("SECRET failure")
				}
				return "SECRET result", nil
			}))
			ag, overrides := requiredToolAgent(t, f)
			r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
			require.NoError(t, err)
			require.Equal(t, verification.StatusPassed, r.VerificationStatus)
			require.NotEmpty(t, hooks.seen)
			require.NotContains(t, fmt.Sprint(hooks.seen), "SECRET")
			if mode == "denied" || mode == "unknown" {
				require.Zero(t, dispatched)
			} else {
				require.Equal(t, 1, dispatched)
			}
			if mode != "completed" {
				steps, readErr := f.storage.ListSteps(f.ctx, r.ID)
				require.NoError(t, readErr)
				rows, readErr := f.storage.ListToolCalls(f.ctx, steps[0].ID)
				require.NoError(t, readErr)
				require.Len(t, rows, 1)
				require.NotEmpty(t, rows[0].Error)
			}
			assertRequiredToolPrivacy(t, f, ag)
		})
	}
}

func TestRequiredVerificationStreamHandoffHidesArguments(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredToolClient{}
	auth := &requiredToolAuthority{}
	e := requiredToolEngine(t, f, client, auth, engine.WithExternalTool(llm.Tool{Name: "probe"}))
	ag, overrides := requiredToolAgent(t, f)
	events := make(chan engine.StreamEvent, 20)
	require.NoError(t, e.StreamAgent(f.ctx, ag.Name, "work", overrides, events))
	paused := false
	for event := range events {
		raw, err := json.Marshal(event)
		require.NoError(t, err)
		require.NotContains(t, string(raw), "SECRET")
		if event.Type == engine.EventSuspended {
			paused = true
		}
	}
	require.True(t, paused)
	rows, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	calls, err := e.PendingVerificationTools(f.ctx, rows[0].ID)
	require.NoError(t, err)
	require.Len(t, calls, 1)
	events = make(chan engine.StreamEvent, 20)
	require.NoError(t, e.ResumeStream(f.ctx, rows[0].ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: calls[0].ID, Error: "SECRET external failure"}}}, events))
	for event := range events {
		raw, marshalErr := json.Marshal(event)
		require.NoError(t, marshalErr)
		require.NotContains(t, string(raw), "SECRET")
	}
	require.Equal(t, 2, client.calls)
	assertRequiredToolPrivacy(t, f, ag)
}

func TestRequiredVerificationApprovalExecutesOnce(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredToolClient{}
	auth := &requiredToolAuthority{approval: true}
	calls := 0
	e := requiredToolEngine(t, f, client, auth, engine.WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) {
		calls++
		return "SECRET approved result", nil
	}))
	ag, overrides := requiredToolAgent(t, f)
	r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, r.State)
	require.Zero(t, calls)
	pending, err := e.ListPendingCheckpoints(f.ctx, nil)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.NoError(t, e.ResolveCheckpoint(f.ctx, pending[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"}))
	saved, err := f.storage.GetRun(f.ctx, r.ID)
	require.NoError(t, err)
	require.Equal(t, verification.StatusPassed, saved.VerificationStatus)
	require.Equal(t, 1, calls)
	require.Equal(t, 1, saved.Usage.ToolCalls)
	_ = e.ResolveCheckpoint(f.ctx, pending[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"})
	require.Equal(t, 1, calls)
	assertRequiredToolPrivacy(t, f, ag)
}

func TestRequiredVerificationResumeRejectsChangedPolicy(t *testing.T) {
	for _, mode := range []string{"revision", "missing handoff", "tampered budget", "downgrade"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredToolClient{}
			auth := &requiredToolAuthority{}
			e := requiredToolEngine(t, f, client, auth, engine.WithExternalTool(llm.Tool{Name: "probe"}))
			ag, overrides := requiredToolAgent(t, f)
			r, err := e.RunAgent(f.ctx, ag.Name, "work", overrides)
			require.NoError(t, err)
			cfg := requiredConfiguration(t, f)
			cfg.HandoffAuthorizer = auth
			if mode == "revision" {
				cfg.Revision = "changed"
			}
			if mode == "missing handoff" {
				cfg.HandoffAuthorizer = nil
			}
			if mode == "tampered budget" || mode == "downgrade" {
				s, readErr := f.storage.GetSuspension(f.ctx, r.ID)
				require.NoError(t, readErr)
				if mode == "tampered budget" {
					s.Cont.Config.Budget.MaxTokens++
				} else {
					s.Cont.SchemaVersion = 2
				}
				require.NoError(t, f.storage.DeleteSuspension(f.ctx, r.ID))
				require.NoError(t, f.storage.CreateSuspension(f.ctx, s))
			}
			after := &requiredToolClient{final: true}
			fresh, newErr := engine.New(engine.WithStore(f.storage), engine.WithLLM(after), engine.WithModelRevision("smart", "model:1"), engine.WithToolRevision("probe", "1"), engine.WithToolAuthorizer(auth), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()), engine.WithVerificationWorker(f.config), engine.WithRequiredVerification(cfg), engine.WithExternalTool(llm.Tool{Name: "probe"}))
			require.NoError(t, newErr)
			_, err = fresh.Resume(f.ctx, r.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "SECRET-ID", Content: "SECRET result"}}})
			require.Error(t, err)
			require.NotContains(t, err.Error(), "SECRET")
			require.Zero(t, after.calls)
			saved, err := f.storage.GetRun(f.ctx, r.ID)
			require.NoError(t, err)
			require.Equal(t, r.Revision, saved.Revision)
			require.Equal(t, run.StatePaused, saved.State)
		})
	}
}
