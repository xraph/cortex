package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
)

// nativeContent is what a thinking model returns next to a tool call: its
// signed thinking block and the tool_use block, compact as the provider
// sends them.
func nativeContent() *llm.NativeContent {
	return &llm.NativeContent{
		Provider: "anthropic",
		Model:    "claude-test",
		Content:  json.RawMessage(`[{"type":"thinking","thinking":"plan","signature":"sig-1"},{"type":"tool_use","id":"call-c","name":"c","input":{}}]`),
	}
}

// nativeLLM answers each call with the next scripted response and keeps a
// deep copy of every request, Metadata and Native included. Streamed
// answers carry their Native on the final chunk only, as nexus does.
type nativeLLM struct {
	mu        sync.Mutex
	responses []*llm.Response
	requests  []*llm.Request
}

func cloneRequest(req *llm.Request) *llm.Request {
	out := *req
	out.Metadata = maps.Clone(req.Metadata)
	out.Messages = make([]llm.Message, len(req.Messages))
	for i, m := range req.Messages {
		m.ToolCalls = slices.Clone(m.ToolCalls)
		if m.Native != nil {
			n := *m.Native
			n.Content = slices.Clone(n.Content)
			m.Native = &n
		}
		out.Messages[i] = m
	}
	return &out
}

func (m *nativeLLM) next(req *llm.Request) *llm.Response {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, cloneRequest(req))
	if n := len(m.requests) - 1; n < len(m.responses) {
		return m.responses[n]
	}
	return &llm.Response{Content: "done", FinishReason: "stop"}
}

func (m *nativeLLM) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	return m.next(req), nil
}

func (m *nativeLLM) CompleteStream(_ context.Context, req *llm.Request) (llm.Stream, error) {
	resp := m.next(req)
	return &chunkStream{chunks: []*llm.Chunk{
		{Content: resp.Content, ToolCalls: resp.ToolCalls},
		{FinishReason: resp.FinishReason, Native: resp.Native},
	}}, nil
}

func (m *nativeLLM) seen() []*llm.Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.requests)
}

// chunkStream yields its chunks, then err (io.EOF when err is nil).
type chunkStream struct {
	chunks []*llm.Chunk
	err    error
	next   int
}

func (s *chunkStream) Next(context.Context) (*llm.Chunk, error) {
	if s.next < len(s.chunks) {
		c := s.chunks[s.next]
		s.next++
		return c, nil
	}
	if s.err != nil {
		return nil, s.err
	}
	return nil, io.EOF
}

func (*chunkStream) Close() error      { return nil }
func (*chunkStream) Usage() *llm.Usage { return &llm.Usage{TotalTokens: 1} }

func nativeToolCall() *llm.Response {
	return &llm.Response{
		ToolCalls:    []llm.ToolCall{{ID: "call-c", Name: "c", Arguments: `{}`}},
		FinishReason: "tool_calls",
		Native:       nativeContent(),
	}
}

// nativeEngine is an engine over SQLite with an agent named assistant and
// the tool c, which approveC sends for approval when gated is set.
func nativeEngine(t *testing.T, model llm.Client, gated bool) (*engine.Engine, *atomic.Int32) {
	t.Helper()
	st := openSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "native.db")+"?_pragma=busy_timeout(10000)")
	var ran atomic.Int32
	base := []engine.Option{
		engine.WithStore(st),
		engine.WithLLM(model),
		engine.WithTool(llm.Tool{Name: "c"}, func(context.Context, cortex.Invocation) (string, error) {
			ran.Add(1)
			return "c-result", nil
		}),
	}
	if gated {
		base = append(base, engine.WithToolAuthorizer(approveC{}), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[leasePrincipal]()))
	}
	e, err := engine.New(base...)
	require.NoError(t, err)
	require.NoError(t, e.CreateAgent(leaseScope(), &agent.Config{ID: id.NewAgentID(), Name: "assistant", MaxSteps: 5}))
	return e, &ran
}

// streamRun runs the assistant through StreamAgent and returns its events.
func streamRun(t *testing.T, e *engine.Engine) []engine.StreamEvent {
	t.Helper()
	events := make(chan engine.StreamEvent, 64)
	require.NoError(t, e.StreamAgent(leaseScope(), "assistant", "work", nil, events))
	var out []engine.StreamEvent
	for ev := range events {
		out = append(out, ev)
	}
	return out
}

func onlyNativeRun(t *testing.T, e *engine.Engine) *run.Run {
	t.Helper()
	rows, err := e.ListRuns(leaseScope(), &run.ListFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	return rows[0]
}

// requireNative checks that the request's assistant message with tool
// calls carries want as cortex holds it, and returns its index. A client
// may re-encode the JSON on the way to the provider, so the model sees
// equivalent content, not necessarily the same bytes.
func requireNative(t *testing.T, req *llm.Request, want *llm.NativeContent) int {
	t.Helper()
	for i, m := range req.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			require.NotNil(t, m.Native, "the assistant message lost its native content")
			require.Equal(t, want.Provider, m.Native.Provider)
			require.Equal(t, want.Model, m.Native.Model)
			require.Equal(t, []byte(want.Content), []byte(m.Native.Content))
			return i
		}
	}
	t.Fatal("the request has no assistant message with tool calls")
	return -1
}

func TestNativeContentSurvivesSuspension(t *testing.T) {
	for _, mode := range []string{"sync", "stream"} {
		t.Run(mode, func(t *testing.T) {
			model := &nativeLLM{responses: []*llm.Response{nativeToolCall()}}
			e, ran := nativeEngine(t, model, true)
			ctx := leaseScope()

			if mode == "sync" {
				paused, err := e.RunAgent(ctx, "assistant", "work", nil)
				require.NoError(t, err)
				require.Equal(t, run.StatePaused, paused.State)
			} else {
				streamRun(t, e)
			}
			r := onlyNativeRun(t, e)
			require.Equal(t, run.StatePaused, r.State)
			cps, err := e.ListPendingCheckpoints(ctx, &checkpoint.ListFilter{RunID: r.ID.String()})
			require.NoError(t, err)
			require.Len(t, cps, 1)
			require.NoError(t, e.DecideCheckpoint(ctx, cps[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))

			done, err := e.ContinueRun(ctx, r.ID)
			require.NoError(t, err)
			require.Equal(t, run.StateCompleted, done.State)
			require.EqualValues(t, 1, ran.Load())

			reqs := model.seen()
			require.Len(t, reqs, 2)
			at := requireNative(t, reqs[1], nativeContent())
			require.Equal(t, len(reqs[0].Messages), at, "the assistant message is not the one appended after the first request")
			require.Equal(t, reqs[0].Messages, reqs[1].Messages[:at], "earlier messages changed across the pause")
		})
	}
}

func TestNativeContentSurvivesReclaim(t *testing.T) {
	first := nativeToolCall()
	first.ToolCalls = []llm.ToolCall{
		{ID: "call-a", Name: "a", Arguments: `{}`},
		{ID: "call-b", Name: "b", Arguments: `{}`},
	}
	model := newLeaseLLM(first)
	a := &countingTool{result: "a-result"}
	stuck := newBlockingTool()
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL),
		engine.WithTool(llm.Tool{Name: "a"}, a.handler),
		engine.WithTool(llm.Tool{Name: "b"}, stuck.handler),
		engine.WithToolRetrySafe("a", "b"))
	t.Cleanup(stuck.letGo)
	f.start()
	stuck.waitEntered(t)
	r := f.onlyRun(t)
	stall(t, f, r.ID)

	// The snapshot the stalled worker left, read back from the store, holds
	// the assistant message with its native content.
	saved, err := f.store.GetProgress(leaseScope(), r.ID)
	require.NoError(t, err)
	requireNative(t, &llm.Request{Messages: saved.Cont.Messages}, first.Native)

	bModel := &nativeLLM{}
	bA, bB := &countingTool{result: "a-again"}, &countingTool{result: "b-result"}
	b := reclaimer(t, f, bModel, newReclaimHooks(),
		engine.WithTool(llm.Tool{Name: "a"}, bA.handler),
		engine.WithTool(llm.Tool{Name: "b"}, bB.handler),
		engine.WithToolRetrySafe("a", "b"))
	engine.SweepOnce(b, context.Background())

	continued, err := b.ContinueRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, continued.State)
	reqs := bModel.seen()
	require.NotEmpty(t, reqs)
	at := requireNative(t, reqs[0], first.Native)
	require.Equal(t, []llm.Message{{Role: "user", Content: "work"}}, reqs[0].Messages[:at], "earlier messages changed across the reclaim")
	require.Equal(t, first.ToolCalls, reqs[0].Messages[at].ToolCalls)
}

func TestNativeNotSavedToMemory(t *testing.T) {
	final := &llm.Response{Content: "all done", FinishReason: "stop", Native: &llm.NativeContent{Provider: "anthropic", Model: "claude-test", Content: json.RawMessage(`[{"type":"thinking","thinking":"wrap up","signature":"sig-2"},{"type":"text","text":"all done"}]`)}}
	model := &nativeLLM{responses: []*llm.Response{nativeToolCall(), final}}
	e, _ := nativeEngine(t, model, false)
	ctx := leaseScope()

	done, err := e.RunAgent(ctx, "assistant", "work", nil)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)

	history, err := e.LoadConversation(ctx, done.AgentID, done.SessionID, 100)
	require.NoError(t, err)
	var assistant int
	for _, m := range history {
		if m.Role != "assistant" {
			continue
		}
		assistant++
		data, marshalErr := json.Marshal(m)
		require.NoError(t, marshalErr)
		require.NotContains(t, string(data), "sig-", "a thinking signature reached conversation memory")
		require.NotContains(t, string(data), "thinking")
	}
	require.Equal(t, 2, assistant)

	// The next run reads that history back with no native content on it.
	_, err = e.RunAgent(ctx, "assistant", "again", nil)
	require.NoError(t, err)
	reqs := model.seen()
	require.Len(t, reqs, 3)
	require.Greater(t, len(reqs[2].Messages), 1, "the second run saw no history")
	for _, m := range reqs[2].Messages {
		require.Nil(t, m.Native, "history from memory carried native content")
	}
}

// blockedLLM streams a tool call, then the provider's guard blocks the
// rest. Complete is blocked outright.
type blockedLLM struct{ calls atomic.Int32 }

var errGuard = errors.New("guard: pii")

func (m *blockedLLM) Complete(context.Context, *llm.Request) (*llm.Response, error) {
	m.calls.Add(1)
	return nil, fmt.Errorf("%w: %w", llm.ErrContentBlocked, errGuard)
}

func (m *blockedLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	m.calls.Add(1)
	return &chunkStream{
		chunks: []*llm.Chunk{{ToolCalls: []llm.ToolCall{{ID: "call-c", Name: "c", Arguments: `{}`}}}},
		err:    fmt.Errorf("%w: %w", llm.ErrContentBlocked, errGuard),
	}, nil
}

func TestBlockedStreamDispatchesNoTool(t *testing.T) {
	for _, mode := range []string{"stream", "sync"} {
		t.Run(mode, func(t *testing.T) {
			model := &blockedLLM{}
			e, ran := nativeEngine(t, model, false)

			if mode == "stream" {
				events := streamRun(t, e)
				types := make([]engine.StreamEventType, 0, len(events))
				for _, ev := range events {
					types = append(types, ev.Type)
					require.NotEqual(t, engine.EventToolCall, ev.Type, "a tool call from a blocked stream was announced")
				}
				require.Contains(t, types, engine.EventSafetyBlock)
			} else {
				_, err := e.RunAgent(leaseScope(), "assistant", "work", nil)
				require.Error(t, err)
				require.NotContains(t, err.Error(), "pii", "the guard's reason leaked into the run error")
			}

			require.Zero(t, ran.Load(), "a tool from a blocked response ran")
			require.EqualValues(t, 1, model.calls.Load())
			r := onlyNativeRun(t, e)
			require.Equal(t, run.StopSafetyBlock, r.StopReason)
			require.NotContains(t, r.Error, "pii")
		})
	}
}

func TestEngineSetsRequestMetadata(t *testing.T) {
	for _, mode := range []string{"sync", "stream"} {
		t.Run(mode, func(t *testing.T) {
			model := &nativeLLM{responses: []*llm.Response{nativeToolCall()}}
			e, _ := nativeEngine(t, model, false)
			if mode == "sync" {
				_, err := e.RunAgent(leaseScope(), "assistant", "work", nil)
				require.NoError(t, err)
			} else {
				streamRun(t, e)
			}
			r := onlyNativeRun(t, e)
			require.Equal(t, run.StateCompleted, r.State)

			reqs := model.seen()
			require.Len(t, reqs, 2)
			for _, req := range reqs {
				require.Equal(t, r.ID.String(), req.Metadata[llm.MetadataRun])
				require.Equal(t, r.AgentID.String(), req.Metadata[llm.MetadataAgent])
				require.True(t, strings.HasPrefix(req.Metadata[llm.MetadataRun], "arun_"))
			}
		})
	}
}

// redactingGate rewrites any text that contains "secret" and approves the rest.
type redactingGate struct{}

func (redactingGate) Check(_ context.Context, r safety.PublicationRequest) (safety.PublicationDecision, error) {
	if strings.Contains(r.Content, "secret") {
		return safety.PublicationDecision{Text: "[redacted]", Status: safety.StatusRedacted}, nil
	}
	return safety.PublicationDecision{Text: r.Content, Status: safety.StatusApproved}, nil
}

// TestRedactedTurnKeepsNativeForReplay pins what the engine sends after a
// publication gate redacts an assistant turn's text: the redacted text with
// the model's native content still attached. Whether the model sees the
// native thinking is the client's call (nexus drops it for a turn whose text
// no longer matches), so the test stops at the request and checks the run
// goes on without error.
func TestRedactedTurnKeepsNativeForReplay(t *testing.T) {
	for _, mode := range []string{"sync", "stream"} {
		t.Run(mode, func(t *testing.T) {
			first := nativeToolCall()
			first.Content = "the secret is 1234"
			model := &nativeLLM{responses: []*llm.Response{first}}
			e := gatedNativeEngine(t, model)

			var done *run.Run
			if mode == "sync" {
				var err error
				done, err = e.RunAgent(leaseScope(), "assistant", "work", nil)
				require.NoError(t, err)
			} else {
				streamRun(t, e)
				done = onlyNativeRun(t, e)
			}
			require.Equal(t, run.StateCompleted, done.State)

			reqs := model.seen()
			require.Len(t, reqs, 2)
			at := requireNative(t, reqs[1], nativeContent())
			assistant := reqs[1].Messages[at]
			require.Equal(t, "[redacted]", assistant.Content, "the replayed turn carried unredacted text")
			require.NotContains(t, assistant.Content, "secret")
		})
	}
}

func gatedNativeEngine(t *testing.T, model llm.Client) *engine.Engine {
	t.Helper()
	st := openSQLiteStoreForTest(t, filepath.Join(t.TempDir(), "gated.db")+"?_pragma=busy_timeout(10000)")
	e, err := engine.New(
		engine.WithStore(st),
		engine.WithLLM(model),
		engine.WithPublicationGate(redactingGate{}),
		engine.WithTool(llm.Tool{Name: "c"}, func(context.Context, cortex.Invocation) (string, error) { return "c-result", nil }),
	)
	require.NoError(t, err)
	require.NoError(t, e.CreateAgent(leaseScope(), &agent.Config{ID: id.NewAgentID(), Name: "assistant", MaxSteps: 5}))
	return e
}
