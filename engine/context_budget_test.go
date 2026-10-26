package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/verification"
)

// Byte costs are deterministic fixtures, not a qualified provider tokenizer.
func fixtureContextCounter(_ context.Context, req *llm.Request) (int, error) {
	n := 10 + len(req.System)
	for _, msg := range req.Messages {
		n += 10 + len(msg.Content)
		for _, call := range msg.ToolCalls {
			n += len(call.ID) + len(call.Name) + len(call.Arguments)
		}
	}
	for _, tool := range req.Tools {
		data, err := json.Marshal(tool)
		if err != nil {
			return 0, err
		}
		n += len(data)
	}
	return n, nil
}

func contextBudgetFixture() engine.ContextBudgetConfig {
	return engine.ContextBudgetConfig{Policy: cognitive.ContextPolicy{Revision: "history/v1", CounterRevision: "fixture-bytes/v1", MaxTokens: 4000, ReservedTokens: 64}, CountTokens: fixtureContextCounter}
}

type contextBudgetClient struct {
	recordingLLM
	calls int
	next  func(*llm.Request, int) *llm.Response
}

func (c *contextBudgetClient) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	c.calls++
	if c.next != nil {
		return c.next(req, c.calls), nil
	}
	return c.recordingLLM.Complete(ctx, req)
}

func (c *contextBudgetClient) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	response, err := c.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	finish := response.FinishReason
	if finish == "" {
		finish = "stop"
	}
	return &requiredStream{chunks: []llm.Chunk{{Content: response.Content, ToolCalls: response.ToolCalls, FinishReason: finish}}}, nil
}

func TestContextBudgetSelectsHistoryAndPersistsOmissions(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[stream], func(t *testing.T) {
			ctx := overlayCtx("context-selection")
			st := newSQLiteStoreForTest(t)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "context", SystemPrompt: "Keep the task constraints."}
			require.NoError(t, st.Create(ctx, ag))
			sid := id.NewSessionID()
			require.NoError(t, st.CreateSession(ctx, &session.Session{ID: sid, AgentID: ag.ID}))
			require.NoError(t, st.SaveConversation(ctx, ag.ID, sid, []memory.Message{{Role: "user", Content: strings.Repeat("old", 2000)}, {Role: "assistant", Content: "old answer"}, {Role: "user", Content: "retain constraint"}, {Role: "assistant", Content: "understood"}}))
			config := contextBudgetFixture()
			config.OptionalHistory = func(_ context.Context, subject cortex.Subject, groups [][]llm.Message) ([]int, error) {
				require.Equal(t, ag.ID, subject.AgentID)
				require.Equal(t, cortex.ScopeFromContext(ctx), subject.Scope)
				require.Len(t, groups, 2)
				groups[1][0].Content = "mutated callback copy"
				return []int{0}, nil
			}
			client := &contextBudgetClient{}
			e, err := engine.New(engine.WithStore(st), engine.WithLLM(client), engine.WithContextBudget(config))
			require.NoError(t, err)
			p, err := e.PrepareAgent(ctx, ag.ID, nil)
			require.NoError(t, err)
			config.Policy.MaxTokens = 1
			var current *run.Run
			if stream {
				events := make(chan engine.StreamEvent, 32)
				require.NoError(t, e.StreamPrepared(ctx, p, "current task", engine.PreparedRunOptions{SessionID: sid}, events))
				for event := range events {
					require.NotEqual(t, engine.EventError, event.Type)
				}
				runs, readErr := st.ListRuns(ctx, nil)
				require.NoError(t, readErr)
				require.Len(t, runs, 1)
				current = runs[0]
			} else {
				current, err = e.RunPrepared(ctx, p, "current task", engine.PreparedRunOptions{SessionID: sid})
				require.NoError(t, err)
			}
			require.Equal(t, run.StateCompleted, current.State)
			require.Equal(t, 1, client.calls)
			req := client.Request(t)
			require.Equal(t, []llm.Message{{Role: "user", Content: "retain constraint"}, {Role: "assistant", Content: "understood"}, {Role: "user", Content: "current task"}}, req.Messages)
			require.Equal(t, 64, req.MaxTokens)
			saved, readErr := st.GetRun(ctx, current.ID)
			require.NoError(t, readErr)
			require.NotNil(t, saved.Provenance.ContextSelection)
			require.Len(t, saved.Provenance.ContextSelection.Selection.Omitted, 1)
			require.NoError(t, saved.Provenance.ContextSelection.Validate())
		})
	}
}

func TestContextBudgetRejectsRequiredHistoryBeforeRunCreation(t *testing.T) {
	ctx := overlayCtx("required-history")
	st := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "context"}
	require.NoError(t, st.Create(ctx, ag))
	sid := id.NewSessionID()
	require.NoError(t, st.CreateSession(ctx, &session.Session{ID: sid, AgentID: ag.ID}))
	require.NoError(t, st.SaveConversation(ctx, ag.ID, sid, []memory.Message{{Role: "user", Content: strings.Repeat("constraint", 1000)}}))
	client := &contextBudgetClient{}
	e, err := engine.New(engine.WithStore(st), engine.WithLLM(client), engine.WithContextBudget(contextBudgetFixture()))
	require.NoError(t, err)
	current, err := e.RunAgent(ctx, ag.Name, "task", &engine.RunOverrides{SessionID: sid})
	require.ErrorIs(t, err, cognitive.ErrContextBudgetExceeded)
	require.Nil(t, current)
	require.Zero(t, client.calls)
	runs, err := st.ListRuns(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, runs)
}

func TestContextBudgetStopsGrowthBeforeAnotherProviderCall(t *testing.T) {
	ctx := overlayCtx("context-growth")
	st := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "context", Tools: []string{"large"}}
	require.NoError(t, st.Create(ctx, ag))
	client := &contextBudgetClient{next: func(_ *llm.Request, call int) *llm.Response {
		require.Equal(t, 1, call)
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call", Name: "large", Arguments: `{}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}
	}}
	e, err := engine.New(engine.WithStore(st), engine.WithLLM(client), engine.WithContextBudget(contextBudgetFixture()), engine.WithTool(llm.Tool{Name: "large"}, func(context.Context, cortex.Invocation) (string, error) { return strings.Repeat("x", 5000), nil }))
	require.NoError(t, err)
	current, err := e.RunAgent(ctx, ag.Name, "task", nil)
	require.ErrorIs(t, err, cognitive.ErrContextBudgetExceeded)
	require.NotNil(t, current)
	require.Equal(t, run.StateIncomplete, current.State)
	require.Equal(t, 1, client.calls)
	require.Equal(t, 1, current.Usage.Steps)
	require.Equal(t, 10, current.Usage.Tokens)
}

func TestContextBudgetRefusesChangedPreparedPolicy(t *testing.T) {
	ctx := overlayCtx("context-policy")
	st := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "context"}
	require.NoError(t, st.Create(ctx, ag))
	config := contextBudgetFixture()
	e, err := engine.New(engine.WithStore(st), engine.WithLLM(&contextBudgetClient{}), engine.WithContextBudget(config))
	require.NoError(t, err)
	p, err := e.PrepareAgent(ctx, ag.ID, nil)
	require.NoError(t, err)
	config.Policy.CounterRevision = "replacement-counter"
	client := &contextBudgetClient{}
	fresh, err := engine.New(engine.WithStore(st), engine.WithLLM(client), engine.WithContextBudget(config))
	require.NoError(t, err)
	_, err = fresh.RunPrepared(ctx, p, "task", engine.PreparedRunOptions{})
	require.ErrorIs(t, err, cortex.ErrContextUnavailable)
	require.Zero(t, client.calls)
}

func TestContextBudgetCounterFailureCannotLeakOrDispatch(t *testing.T) {
	ctx := overlayCtx("context-counter")
	st := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "context"}
	require.NoError(t, st.Create(ctx, ag))
	config := contextBudgetFixture()
	config.CountTokens = func(context.Context, *llm.Request) (int, error) { return 0, errors.New("private tokenizer details") }
	client := &contextBudgetClient{}
	e, err := engine.New(engine.WithStore(st), engine.WithLLM(client), engine.WithContextBudget(config))
	require.NoError(t, err)
	_, err = e.RunAgent(ctx, ag.Name, "task", nil)
	require.ErrorIs(t, err, cortex.ErrContextUnavailable)
	require.NotContains(t, err.Error(), "private")
	require.Zero(t, client.calls)
}

func TestContextBudgetRequiredEvidenceAndApprovalResume(t *testing.T) {
	for _, mode := range []string{"sync", "stream", "approval_resume"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredToolClient{}
			auth := &requiredToolAuthority{approval: mode == "approval_resume"}
			config := contextBudgetFixture()
			classifications := 0
			config.OptionalHistory = func(context.Context, cortex.Subject, [][]llm.Message) ([]int, error) {
				classifications++
				return []int{0}, nil
			}
			options := []engine.Option{engine.WithContextBudget(config), engine.WithTool(namedTool("probe"), noopHandler)}
			e := requiredToolEngine(t, f, client, auth, options...)
			ag, overrides := requiredToolAgent(t, f)
			sid := id.NewSessionID()
			require.NoError(t, f.storage.CreateSession(f.ctx, &session.Session{ID: sid, AgentID: ag.ID}))
			original := []memory.Message{{Role: "user", Content: strings.Repeat("private-old", 500)}, {Role: "assistant", Content: "old reply"}, {Role: "user", Content: "retain source constraint"}, {Role: "assistant", Content: "accepted"}}
			require.NoError(t, f.storage.SaveConversation(f.ctx, ag.ID, sid, original))
			overrides.SessionID = sid
			var current *run.Run
			if mode == "stream" {
				events := make(chan engine.StreamEvent, 32)
				require.NoError(t, e.StreamAgent(f.ctx, ag.Name, "task", overrides, events))
				for event := range events {
					data, err := json.Marshal(event)
					require.NoError(t, err)
					require.NotContains(t, string(data), "private-old")
				}
				rows, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
				require.NoError(t, err)
				require.Len(t, rows, 1)
				current = rows[0]
			} else {
				var err error
				current, err = e.RunAgent(f.ctx, ag.Name, "task", overrides)
				require.NoError(t, err)
			}
			if mode == "approval_resume" {
				require.Equal(t, run.StatePaused, current.State)
				client = &requiredToolClient{final: true}
				fresh := requiredToolEngine(t, f, client, auth, options...)
				pending, err := fresh.ListPendingCheckpoints(f.ctx, nil)
				require.NoError(t, err)
				require.Len(t, pending, 1)
				require.NoError(t, fresh.ResolveCheckpoint(f.ctx, pending[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"}))
				current, err = f.storage.GetRun(f.ctx, current.ID)
				require.NoError(t, err)
			}
			require.Equal(t, verification.StatusPassed, current.VerificationStatus)
			require.Equal(t, 1, classifications, "resume must not reselect history using a new callback result")
			require.Equal(t, "retain source constraint", client.last.Messages[0].Content)
			intent, err := f.storage.GetRunCaptureIntent(f.ctx, current.ID)
			require.NoError(t, err)
			artifact, err := f.storage.LoadEvidence(f.ctx, *intent.Template.Binding.Context.ExecutionInputs)
			require.NoError(t, err)
			inputs, err := verification.DecodeExecutionInputs(artifact.Data)
			require.NoError(t, err)
			require.Equal(t, 2, inputs.SchemaVersion)
			require.Len(t, inputs.OriginalHistory, 4)
			require.Len(t, inputs.History, 2)
			require.Equal(t, original[0].Content, inputs.OriginalHistory[0].Content)
			require.NoError(t, current.ValidateExecutionInputs(inputs))
			inputs.History[0].Content = "tampered constraint"
			_, err = inputs.Encode()
			require.ErrorIs(t, err, verification.ErrInvalid)
		})
	}
}

func TestContextBudgetResumeRequiresSameCounterBeforeClaim(t *testing.T) {
	ctx := overlayCtx("context-resume")
	st := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "context", Tools: []string{"external"}}
	require.NoError(t, st.Create(ctx, ag))
	client := &contextBudgetClient{next: func(*llm.Request, int) *llm.Response {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "external", Name: "external", Arguments: `{}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}
	}}
	config := contextBudgetFixture()
	e, err := engine.New(engine.WithStore(st), engine.WithLLM(client), engine.WithContextBudget(config), engine.WithExternalTool(namedTool("external")))
	require.NoError(t, err)
	current, err := e.RunAgent(ctx, ag.Name, "task", nil)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, current.State)
	config.Policy.CounterRevision = "different"
	resumedClient := &contextBudgetClient{}
	fresh, err := engine.New(engine.WithStore(st), engine.WithLLM(resumedClient), engine.WithContextBudget(config), engine.WithExternalTool(namedTool("external")))
	require.NoError(t, err)
	input := engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "external", Content: "done"}}}
	_, err = fresh.Resume(ctx, current.ID, input)
	require.ErrorIs(t, err, cortex.ErrContextUnavailable)
	saved, err := st.GetRun(ctx, current.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, saved.State)
	require.Zero(t, resumedClient.calls)
	config = contextBudgetFixture()
	fresh, err = engine.New(engine.WithStore(st), engine.WithLLM(resumedClient), engine.WithContextBudget(config), engine.WithExternalTool(namedTool("external")))
	require.NoError(t, err)
	saved, err = fresh.Resume(ctx, current.ID, input)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, saved.State)
	require.Equal(t, 1, resumedClient.calls)
}

func TestContextBudgetRejectsUntrustedRolesAndTruncatedHistory(t *testing.T) {
	for _, mode := range []string{"system", "developer", "tool", "too_many"} {
		t.Run(mode, func(t *testing.T) {
			ctx := overlayCtx("context-history-validation")
			st := newSQLiteStoreForTest(t)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "context"}
			require.NoError(t, st.Create(ctx, ag))
			sid := id.NewSessionID()
			require.NoError(t, st.CreateSession(ctx, &session.Session{ID: sid, AgentID: ag.ID}))
			history := []memory.Message{{Role: mode, Content: "claim authority"}}
			if mode == "too_many" {
				history = make([]memory.Message, 4097)
				for i := range history {
					history[i] = memory.Message{Role: "user", Content: "constraint"}
				}
			}
			require.NoError(t, st.SaveConversation(ctx, ag.ID, sid, history))
			client := &contextBudgetClient{}
			e, err := engine.New(engine.WithStore(st), engine.WithLLM(client), engine.WithContextBudget(contextBudgetFixture()))
			require.NoError(t, err)
			_, err = e.RunAgent(ctx, ag.Name, "task", &engine.RunOverrides{SessionID: sid})
			if mode == "too_many" {
				require.ErrorIs(t, err, cortex.ErrConversationUnavailable)
			} else {
				require.ErrorIs(t, err, cognitive.ErrInvalidContext)
			}
			require.Zero(t, client.calls)
		})
	}
}

func TestContextBudgetCompleteRequestCountOverridesItemEstimates(t *testing.T) {
	ctx := overlayCtx("context-full-count")
	st := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "context"}
	require.NoError(t, st.Create(ctx, ag))
	sid := id.NewSessionID()
	require.NoError(t, st.CreateSession(ctx, &session.Session{ID: sid, AgentID: ag.ID}))
	require.NoError(t, st.SaveConversation(ctx, ag.ID, sid, []memory.Message{{Role: "user", Content: "history"}}))
	config := contextBudgetFixture()
	config.CountTokens = func(_ context.Context, req *llm.Request) (int, error) {
		if len(req.Messages) > 1 {
			return 4001, nil
		}
		return 10, nil
	}
	client := &contextBudgetClient{}
	e, err := engine.New(engine.WithStore(st), engine.WithLLM(client), engine.WithContextBudget(config))
	require.NoError(t, err)
	_, err = e.RunAgent(ctx, ag.Name, "task", &engine.RunOverrides{SessionID: sid})
	require.ErrorIs(t, err, cognitive.ErrContextBudgetExceeded)
	require.Zero(t, client.calls)
}

type alteredContextPauseStore struct {
	*sqlite.Store
	alter    func(*suspension.Suspension)
	failRead bool
}

func (s *alteredContextPauseStore) GetSuspension(ctx context.Context, key id.AgentRunID) (*suspension.Suspension, error) {
	if s.failRead {
		return nil, errors.New("private pause read error")
	}
	paused, err := s.Store.GetSuspension(ctx, key)
	if err == nil {
		s.alter(paused)
	}
	return paused, err
}

func TestContextBudgetRejectsAlteredResumeBeforeClaim(t *testing.T) {
	for _, mode := range []string{"history", "task_fields", "receipt", "missing_preview"} {
		t.Run(mode, func(t *testing.T) {
			ctx := overlayCtx("context-altered-resume")
			st := newSQLiteStoreForTest(t)
			ag := &agent.Config{ID: id.NewAgentID(), Name: "context", Tools: []string{"external"}}
			require.NoError(t, st.Create(ctx, ag))
			sid := id.NewSessionID()
			require.NoError(t, st.CreateSession(ctx, &session.Session{ID: sid, AgentID: ag.ID}))
			require.NoError(t, st.SaveConversation(ctx, ag.ID, sid, []memory.Message{{Role: "user", Content: "original constraint"}}))
			client := &contextBudgetClient{next: func(*llm.Request, int) *llm.Response {
				return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "external", Name: "external", Arguments: `{}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}
			}}
			e, err := engine.New(engine.WithStore(st), engine.WithLLM(client), engine.WithContextBudget(contextBudgetFixture()), engine.WithExternalTool(namedTool("external")))
			require.NoError(t, err)
			current, err := e.RunAgent(ctx, ag.Name, "task", &engine.RunOverrides{SessionID: sid})
			require.NoError(t, err)
			require.Equal(t, run.StatePaused, current.State)
			wrapped := &alteredContextPauseStore{Store: st, failRead: mode == "missing_preview", alter: func(paused *suspension.Suspension) {
				switch mode {
				case "history":
					paused.Cont.Messages[0].Content = "substituted constraint"
				case "task_fields":
					paused.Cont.Messages[paused.Cont.NewMessagesFrom].ToolCallID = "injected association"
				case "receipt":
					paused.Cont.Provenance.ContextSelection.SelectedDigest = strings.Repeat("0", 64)
				}
			}}
			resumed := &contextBudgetClient{}
			fresh, err := engine.New(engine.WithStore(wrapped), engine.WithLLM(resumed), engine.WithContextBudget(contextBudgetFixture()), engine.WithExternalTool(namedTool("external")))
			require.NoError(t, err)
			_, err = fresh.Resume(ctx, current.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "external", Content: "done"}}})
			if mode == "missing_preview" {
				require.ErrorIs(t, err, cortex.ErrContextUnavailable)
			} else {
				require.ErrorIs(t, err, cortex.ErrInvalidContinuation)
			}
			require.Zero(t, resumed.calls)
			saved, err := st.GetRun(ctx, current.ID)
			require.NoError(t, err)
			require.Equal(t, run.StatePaused, saved.State)
		})
	}
}

func TestContextBudgetStreamStopsBeforeSecondDispatch(t *testing.T) {
	ctx := overlayCtx("context-stream-growth")
	st := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "context", Tools: []string{"large"}}
	require.NoError(t, st.Create(ctx, ag))
	client := &contextBudgetClient{next: func(*llm.Request, int) *llm.Response {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call", Name: "large", Arguments: `{}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 5}}
	}}
	e, err := engine.New(engine.WithStore(st), engine.WithLLM(client), engine.WithContextBudget(contextBudgetFixture()), engine.WithTool(namedTool("large"), func(context.Context, cortex.Invocation) (string, error) { return strings.Repeat("x", 5000), nil }))
	require.NoError(t, err)
	events := make(chan engine.StreamEvent, 32)
	require.NoError(t, e.StreamAgent(ctx, ag.Name, "task", nil, events))
	sawFailure := false
	for event := range events {
		require.NotEqual(t, engine.EventDone, event.Type)
		sawFailure = sawFailure || event.Type == engine.EventError
	}
	require.True(t, sawFailure)
	require.Equal(t, 1, client.calls)
	rows, err := st.ListRuns(ctx, nil)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, run.StateIncomplete, rows[0].State)
	require.Equal(t, 1, rows[0].Usage.Steps)
	require.Equal(t, 5, rows[0].Usage.Tokens)
}

func TestContextBudgetCounterCannotMutateProviderRequest(t *testing.T) {
	ctx := overlayCtx("context-counter-copy")
	st := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "context", SystemPrompt: "trusted instructions", Tools: []string{"probe"}}
	require.NoError(t, st.Create(ctx, ag))
	config := contextBudgetFixture()
	config.CountTokens = func(c context.Context, req *llm.Request) (int, error) {
		n, err := fixtureContextCounter(c, req)
		req.System = "changed by counter"
		req.MaxTokens = 999999
		if len(req.Messages) > 0 {
			req.Messages[0].Content = "changed by counter"
		}
		if req.Temperature != nil {
			*req.Temperature = 99
		}
		if len(req.Tools) > 0 {
			params, ok := req.Tools[0].Parameters.(map[string]any)
			require.True(t, ok)
			require.Equal(t, json.Number("9007199254740993"), params["large_integer"])
			params["type"] = "changed by counter"
		}
		return n, err
	}
	client := &contextBudgetClient{}
	tool := llm.Tool{Name: "probe", Parameters: map[string]any{"type": "object", "large_integer": json.Number("9007199254740993")}}
	e, err := engine.New(engine.WithStore(st), engine.WithLLM(client), engine.WithContextBudget(config), engine.WithTool(tool, noopHandler))
	require.NoError(t, err)
	_, err = e.RunAgent(ctx, ag.Name, "task", nil)
	require.NoError(t, err)
	request := client.Request(t)
	require.Equal(t, "trusted instructions", request.System)
	require.Equal(t, "task", request.Messages[0].Content)
	require.Equal(t, 64, request.MaxTokens)
	require.NotEqual(t, float64(99), *request.Temperature)
	params, ok := request.Tools[0].Parameters.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "object", params["type"])
}

func TestContextBudgetPreparedSchemaCannotBeDowngraded(t *testing.T) {
	ctx := overlayCtx("context-schema")
	st := newSQLiteStoreForTest(t)
	ag := &agent.Config{ID: id.NewAgentID(), Name: "context"}
	require.NoError(t, st.Create(ctx, ag))
	e, err := engine.New(engine.WithStore(st), engine.WithLLM(&contextBudgetClient{}), engine.WithContextBudget(contextBudgetFixture()))
	require.NoError(t, err)
	prepared, err := e.PrepareAgent(ctx, ag.ID, nil)
	require.NoError(t, err)
	data, err := json.Marshal(prepared)
	require.NoError(t, err)
	var wire struct {
		Digest   string          `json:"digest"`
		Snapshot json.RawMessage `json:"snapshot"`
	}
	require.NoError(t, json.Unmarshal(data, &wire))
	var snapshot map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(wire.Snapshot, &snapshot))
	snapshot["schema_version"] = json.RawMessage("2")
	wire.Snapshot, err = json.Marshal(snapshot)
	require.NoError(t, err)
	wire.Digest = evidence.Digest(wire.Snapshot)
	data, err = json.Marshal(wire)
	require.NoError(t, err)
	var restored engine.PreparedAgent
	require.ErrorIs(t, json.Unmarshal(data, &restored), cortex.ErrInvalidPrepared)
}
