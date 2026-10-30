package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
)

// testPrincipal is the host identity every test here puts on the context.
type testPrincipal struct {
	User string `json:"user"`
	Org  string `json:"org"`
}

var (
	alice = testPrincipal{User: "alice", Org: "o1"}
	bob   = testPrincipal{User: "bob", Org: "o1"}
	carol = testPrincipal{User: "carol", Org: "o1"}
)

// sequenceLLM answers each Complete with the next scripted response and
// then with plain content, so one run can pause, resume and call another
// tool before it finishes.
type sequenceLLM struct {
	mu        sync.Mutex
	responses []llm.Response
	calls     int
}

func (s *sequenceLLM) Complete(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	if len(s.responses) == 0 {
		return &llm.Response{Content: "done"}, nil
	}
	next := s.responses[0]
	s.responses = s.responses[1:]
	return &next, nil
}

func (s *sequenceLLM) CompleteStream(_ context.Context, _ *llm.Request) (llm.Stream, error) {
	return nil, errors.New("sequenceLLM: CompleteStream not supported")
}

func (s *sequenceLLM) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// callsTool is a model response asking for one tool call.
func callsTool(callID, name string) llm.Response {
	return llm.Response{ToolCalls: []llm.ToolCall{{ID: callID, Name: name, Arguments: `{}`}}}
}

// principalRecorder records the principal every authorizer call saw. It
// escalates one tool for approval and allows the rest.
type principalRecorder struct {
	gated string

	mu        sync.Mutex
	visible   []any
	authorize []any
}

func (a *principalRecorder) Visible(_ context.Context, s cortex.Subject, tools []llm.Tool) []llm.Tool {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.visible = append(a.visible, s.Principal)
	return tools
}

func (a *principalRecorder) Authorize(_ context.Context, s cortex.Subject, call llm.ToolCall) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.authorize = append(a.authorize, s.Principal)
	if call.Name == a.gated {
		return fmt.Errorf("%s needs a person: %w", call.Name, cortex.ErrRequiresApproval)
	}
	return nil
}

// since drops everything recorded so far, so a test reads only what the
// resume saw.
func (a *principalRecorder) since() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.visible, a.authorize = nil, nil
}

func (a *principalRecorder) snapshot() (visible, authorize []any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return slices.Clone(a.visible), slices.Clone(a.authorize)
}

// handlerPrincipals records the principal each tool invocation carried.
type handlerPrincipals struct {
	mu   sync.Mutex
	seen map[string][]any
}

func (h *handlerPrincipals) tool(name string) Option {
	return WithTool(llm.Tool{Name: name, Description: "test tool"}, func(_ context.Context, inv cortex.Invocation) (string, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if h.seen == nil {
			h.seen = make(map[string][]any)
		}
		h.seen[name] = append(h.seen[name], inv.Principal)
		return name + " ok", nil
	})
}

func (h *handlerPrincipals) of(name string) []any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.seen[name])
}

// switchCodec is the JSON codec with a decode failure that can be
// switched on, for a host whose identity service is down or has removed
// the user.
type switchCodec struct {
	cortex.PrincipalCodec
	encodeErr error
	decodeErr atomic.Pointer[error]
}

func newSwitchCodec() *switchCodec {
	return &switchCodec{PrincipalCodec: cortex.JSONPrincipalCodec[testPrincipal]()}
}

func (c *switchCodec) failDecode(err error) { c.decodeErr.Store(&err) }

func (c *switchCodec) EncodePrincipal(ctx context.Context, principal any) ([]byte, error) {
	if c.encodeErr != nil {
		return nil, c.encodeErr
	}
	return c.PrincipalCodec.EncodePrincipal(ctx, principal)
}

func (c *switchCodec) DecodePrincipal(ctx context.Context, data []byte) (any, error) {
	if err := c.decodeErr.Load(); err != nil && *err != nil {
		return nil, *err
	}
	return c.PrincipalCodec.DecodePrincipal(ctx, data)
}

// principalStore is a migrated SQLite store with an agent, steady, that
// has the steps to pause, resume, call one more tool and answer.
func principalStore(ctx context.Context, t *testing.T) *sqlitestore.Store {
	t.Helper()
	st := newApprovalStore(ctx, t)
	if err := st.Create(ctx, &agent.Config{ID: id.NewAgentID(), Name: "steady", MaxSteps: 5}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return st
}

func as(ctx context.Context, p testPrincipal) context.Context {
	return cortex.WithPrincipal(ctx, p)
}

func requireAll(t *testing.T, what string, got []any, want testPrincipal) {
	t.Helper()
	if len(got) == 0 {
		t.Fatalf("%s: no calls recorded after the resume", what)
	}
	for i, p := range got {
		if p != want {
			t.Errorf("%s call %d saw principal %#v, want %#v", what, i, p, want)
		}
	}
}

// An approval is decided by someone other than the person who started
// the run. The decision is theirs; the run is not. Every authorization
// after the resume asks about alice, and the checkpoint records bob.
func TestResumeKeepsOriginalPrincipal(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := principalStore(ctx, t)
	codec := cortex.JSONPrincipalCodec[testPrincipal]()
	auth := &principalRecorder{gated: "deploy"}
	handlers := &handlerPrincipals{}
	e, err := New(WithStore(st), WithToolAuthorizer(auth), WithPrincipalCodec(codec),
		WithLLM(&sequenceLLM{responses: []llm.Response{callsTool("call-1", "deploy"), callsTool("call-2", "probe")}}),
		handlers.tool("deploy"), handlers.tool("probe"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	paused, err := e.RunAgent(as(ctx, alice), "steady", "deploy it", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if paused.State != run.StatePaused {
		t.Fatalf("run state = %q, want paused", paused.State)
	}
	pending, err := e.ListPendingCheckpoints(ctx, &checkpoint.ListFilter{Limit: 10})
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPendingCheckpoints = %d rows, %v; want 1", len(pending), err)
	}

	auth.since()
	if err = e.ResolveCheckpoint(as(ctx, bob), pending[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}); err != nil {
		t.Fatalf("ResolveCheckpoint: %v", err)
	}

	visible, authorize := auth.snapshot()
	requireAll(t, "Authorize", authorize, alice)
	requireAll(t, "Visible", visible, alice)
	requireAll(t, "deploy handler", handlers.of("deploy"), alice)
	requireAll(t, "probe handler", handlers.of("probe"), alice)

	done, err := st.GetRun(ctx, paused.ID)
	if err != nil || done.State != run.StateCompleted {
		t.Fatalf("run after approval = %+v, %v; want completed", done, err)
	}

	decided, err := e.GetCheckpoint(ctx, pending[0].ID)
	if err != nil {
		t.Fatalf("GetCheckpoint: %v", err)
	}
	if decided.Decision == nil || len(decided.Decision.DecidedByPrincipal) == 0 {
		t.Fatalf("checkpoint decision = %+v, want the decider's principal recorded", decided.Decision)
	}
	decider, err := codec.DecodePrincipal(ctx, decided.Decision.DecidedByPrincipal)
	if err != nil {
		t.Fatalf("decode DecidedByPrincipal: %v", err)
	}
	if decider != bob {
		t.Errorf("DecidedByPrincipal decodes to %#v, want %#v", decider, bob)
	}
}

// An external tool's result is posted by whatever service ran it, under
// its own identity. The run carries on as the person who started it.
func TestExternalResumeKeepsOriginalPrincipal(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := principalStore(ctx, t)
	auth := &principalRecorder{}
	handlers := &handlerPrincipals{}
	e, err := New(WithStore(st), WithToolAuthorizer(auth), WithPrincipalCodec(cortex.JSONPrincipalCodec[testPrincipal]()),
		WithLLM(&sequenceLLM{responses: []llm.Response{callsTool("call-1", externalTool().Name), callsTool("call-2", "probe")}}),
		WithExternalTool(externalTool()), handlers.tool("probe"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	paused, err := e.RunAgent(as(ctx, alice), "steady", "ask first", nil)
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("RunAgent = %+v, %v; want a paused run", paused, err)
	}

	auth.since()
	resumed, err := e.Resume(as(ctx, carol), paused.ID, ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-1", Content: "yes"}}})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.State != run.StateCompleted {
		t.Fatalf("resumed run state = %q, want completed", resumed.State)
	}
	visible, authorize := auth.snapshot()
	requireAll(t, "Visible", visible, alice)
	requireAll(t, "Authorize", authorize, alice)
	requireAll(t, "probe handler", handlers.of("probe"), alice)
}

// An authorizer without a codec would see every resumed run as nobody.
// The engine refuses that configuration up front, in either option order.
func TestAuthorizerWithoutCodecFailsNew(t *testing.T) {
	auth := &principalRecorder{}
	if _, err := New(WithToolAuthorizer(auth)); !errors.Is(err, cortex.ErrPrincipalCodecRequired) {
		t.Fatalf("New(WithToolAuthorizer) without a codec = %v, want ErrPrincipalCodecRequired", err)
	}
	codec := cortex.JSONPrincipalCodec[string]()
	if _, err := New(WithPrincipalCodec(codec), WithToolAuthorizer(auth)); err != nil {
		t.Fatalf("New with the codec first: %v", err)
	}
	if _, err := New(WithToolAuthorizer(auth), WithPrincipalCodec(codec)); err != nil {
		t.Fatalf("New with the codec last: %v", err)
	}
	if _, err := New(WithToolAuthorizer(nil)); err != nil {
		t.Fatalf("New(WithToolAuthorizer(nil)) without a codec: %v", err)
	}
}

// With nothing to restore it from, a resumed run has no principal. It
// does not borrow the resumer's.
func TestNoAuthorizerNoCodecResumesWithNilPrincipal(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := principalStore(ctx, t)
	handlers := &handlerPrincipals{}
	e, err := New(WithStore(st),
		WithLLM(&sequenceLLM{responses: []llm.Response{callsTool("call-1", externalTool().Name), callsTool("call-2", "probe")}}),
		WithExternalTool(externalTool()), handlers.tool("probe"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	paused, err := e.RunAgent(as(ctx, alice), "steady", "ask first", nil)
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("RunAgent = %+v, %v; want a paused run", paused, err)
	}
	if _, err = e.Resume(as(ctx, bob), paused.ID, ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-1", Content: "yes"}}}); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	seen := handlers.of("probe")
	if len(seen) != 1 {
		t.Fatalf("probe ran %d times, want 1", len(seen))
	}
	if seen[0] != nil {
		t.Errorf("probe saw principal %#v after the resume, want nil", seen[0])
	}
}

// A principal the codec cannot write is a run that could not be resumed
// as its caller, so it is never created.
func TestPrincipalEncodeFailureRefusesRun(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := principalStore(ctx, t)
	codec := newSwitchCodec()
	codec.encodeErr = errors.New("identity service down")
	e, err := New(WithStore(st), WithPrincipalCodec(codec), WithLLM(&sequenceLLM{}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err = e.RunAgent(as(ctx, alice), "steady", "hello", nil); !errors.Is(err, cortex.ErrPrincipalUnavailable) {
		t.Fatalf("RunAgent with a failing encoder = %v, want ErrPrincipalUnavailable", err)
	}
	if n, countErr := st.CountRuns(ctx, nil); countErr != nil || n != 0 {
		t.Fatalf("CountRuns = %d, %v; want no run created", n, countErr)
	}
}

// A revoked principal ends the run. It never continues under nil, and it
// never continues under whoever resumed it.
func TestRevokedPrincipalFailsResume(t *testing.T) {
	revoked := fmt.Errorf("gone: %w", cortex.ErrPrincipalRevoked)

	t.Run("external", func(t *testing.T) {
		ctx := cortex.WithScope(context.Background(), approvalScope())
		st := principalStore(ctx, t)
		codec := newSwitchCodec()
		handlers := &handlerPrincipals{}
		model := &sequenceLLM{responses: []llm.Response{callsTool("call-1", externalTool().Name), callsTool("call-2", "probe")}}
		logger := log.NewTestLogger()
		e, err := New(WithStore(st), WithToolAuthorizer(&principalRecorder{}), WithPrincipalCodec(codec), WithLogger(logger),
			WithLLM(model), WithExternalTool(externalTool()), handlers.tool("probe"))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		paused, err := e.RunAgent(as(ctx, alice), "steady", "ask first", nil)
		if err != nil || paused.State != run.StatePaused {
			t.Fatalf("RunAgent = %+v, %v; want a paused run", paused, err)
		}
		codec.failDecode(revoked)
		calls := model.callCount()
		_, err = e.Resume(as(ctx, bob), paused.ID, ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-1", Content: "yes"}}})
		if !errors.Is(err, cortex.ErrPrincipalRevoked) {
			t.Fatalf("Resume with a revoked principal = %v, want ErrPrincipalRevoked", err)
		}
		failed, err := st.GetRun(ctx, paused.ID)
		if err != nil || failed.State != run.StateFailed {
			t.Fatalf("run after a revoked resume = %+v, %v; want failed", failed, err)
		}
		if !strings.Contains(failed.Error, "gone") || !strings.Contains(failed.Error, cortex.ErrPrincipalRevoked.Error()) {
			t.Errorf("failed run's error = %q, want the codec's reason and the revocation", failed.Error)
		}
		capture, ok := logger.(*log.TestLogger)
		if !ok {
			t.Fatalf("NewTestLogger returned %T", logger)
		}
		warned := false
		for _, entry := range capture.GetLogs() {
			if entry.Message == "fail a resumed run whose principal was revoked" {
				id, has := entry.Field("run_id")
				warned = has && id == paused.ID.String()
			}
		}
		if !warned {
			t.Errorf("no warning names the run failed for a revoked principal: %+v", capture.GetLogs())
		}
		if got := handlers.of("probe"); len(got) != 0 {
			t.Errorf("probe ran %d times after the principal was revoked", len(got))
		}
		if model.callCount() != calls {
			t.Errorf("the model was called %d more times after the principal was revoked", model.callCount()-calls)
		}
	})

	t.Run("approval", func(t *testing.T) {
		ctx := cortex.WithScope(context.Background(), approvalScope())
		st := principalStore(ctx, t)
		codec := newSwitchCodec()
		handlers := &handlerPrincipals{}
		e, err := New(WithStore(st), WithToolAuthorizer(&principalRecorder{gated: "deploy"}), WithPrincipalCodec(codec),
			WithLLM(&sequenceLLM{responses: []llm.Response{callsTool("call-1", "deploy")}}), handlers.tool("deploy"))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		paused, err := e.RunAgent(as(ctx, alice), "steady", "deploy it", nil)
		if err != nil || paused.State != run.StatePaused {
			t.Fatalf("RunAgent = %+v, %v; want a paused run", paused, err)
		}
		pending, err := e.ListPendingCheckpoints(ctx, &checkpoint.ListFilter{Limit: 10})
		if err != nil || len(pending) != 1 {
			t.Fatalf("ListPendingCheckpoints = %d rows, %v; want 1", len(pending), err)
		}
		codec.failDecode(revoked)
		err = e.ResolveCheckpoint(as(ctx, bob), pending[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"})
		if !errors.Is(err, cortex.ErrPrincipalRevoked) {
			t.Fatalf("ResolveCheckpoint with a revoked principal = %v, want ErrPrincipalRevoked", err)
		}
		failed, err := st.GetRun(ctx, paused.ID)
		if err != nil || failed.State != run.StateFailed {
			t.Fatalf("run after a revoked approval = %+v, %v; want failed", failed, err)
		}
		if got := handlers.of("deploy"); len(got) != 0 {
			t.Errorf("the approved call ran %d times under a revoked principal", len(got))
		}
		if cp, cpErr := e.GetCheckpoint(ctx, pending[0].ID); cpErr != nil || cp.State == checkpointStatePending {
			t.Errorf("checkpoint after a revoked approval = %+v, %v; want it closed with the run", cp, cpErr)
		}
	})
}

// A decode that fails for a reason other than revocation says nothing
// about the run. It stays paused, and the next resume can finish it.
func TestTransientPrincipalErrorLeavesRunPaused(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := principalStore(ctx, t)
	codec := newSwitchCodec()
	handlers := &handlerPrincipals{}
	e, err := New(WithStore(st), WithToolAuthorizer(&principalRecorder{}), WithPrincipalCodec(codec),
		WithLLM(&sequenceLLM{responses: []llm.Response{callsTool("call-1", externalTool().Name), callsTool("call-2", "probe")}}),
		WithExternalTool(externalTool()), handlers.tool("probe"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	paused, err := e.RunAgent(as(ctx, alice), "steady", "ask first", nil)
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("RunAgent = %+v, %v; want a paused run", paused, err)
	}
	in := ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-1", Content: "yes"}}}

	codec.failDecode(errors.New("db down"))
	_, err = e.Resume(as(ctx, bob), paused.ID, in)
	if err == nil {
		t.Fatal("Resume succeeded while the principal could not be decoded")
	}
	if errors.Is(err, cortex.ErrPrincipalRevoked) || !errors.Is(err, cortex.ErrPrincipalUnavailable) {
		t.Fatalf("Resume with a transient decode error = %v, want ErrPrincipalUnavailable and not revoked", err)
	}
	still, err := st.GetRun(ctx, paused.ID)
	if err != nil || still.State != run.StatePaused {
		t.Fatalf("run after a transient decode error = %+v, %v; want still paused", still, err)
	}

	codec.failDecode(nil)
	resumed, err := e.Resume(as(ctx, bob), paused.ID, in)
	if err != nil {
		t.Fatalf("Resume after the decoder recovered: %v", err)
	}
	if resumed.State != run.StateCompleted {
		t.Fatalf("resumed run state = %q, want completed", resumed.State)
	}
	requireAll(t, "probe handler", handlers.of("probe"), alice)
}

// plannerLLM scripts the planner of an a2aEngine and answers every other
// agent with plain content, so a peer that happens to run cannot take
// the planner's next response.
type plannerLLM struct {
	planner *sequenceLLM
}

func (p plannerLLM) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	if strings.Contains(req.System, "you are planner") {
		return p.planner.Complete(ctx, req)
	}
	return &llm.Response{Content: "done"}, nil
}

func (p plannerLLM) CompleteStream(_ context.Context, _ *llm.Request) (llm.Stream, error) {
	return nil, errors.New("plannerLLM: CompleteStream not supported")
}

func askThenProbe() plannerLLM {
	return plannerLLM{planner: &sequenceLLM{responses: []llm.Response{
		{ToolCalls: []llm.ToolCall{{ID: "call-1", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"status?"}`}}},
		callsTool("call-2", "probe"),
	}}}
}

// A peer's reply resumes the asking run on the messaging bus, under
// whatever context delivered it. The run carries on as its own caller.
func TestAgentReplyResumeKeepsOriginalPrincipal(t *testing.T) {
	e, st, ctx := a2aEngine(t, nil)
	auth := &principalRecorder{}
	handlers := &handlerPrincipals{}
	for _, option := range []Option{WithLLM(askThenProbe()), WithToolAuthorizer(auth), WithPrincipalCodec(cortex.JSONPrincipalCodec[testPrincipal]()), handlers.tool("probe")} {
		if err := option(e); err != nil {
			t.Fatalf("option: %v", err)
		}
	}
	current, err := e.RunAgent(as(ctx, alice), "planner", "ask", nil)
	if err != nil || current.State != run.StatePaused {
		t.Fatalf("RunAgent = %+v, %v; want a run paused on its question", current, err)
	}
	messages, err := st.ListMessages(ctx, &a2a.MessageListFilter{Limit: 10})
	if err != nil || len(messages) != 1 {
		t.Fatalf("ListMessages = %d, %v; want the one question", len(messages), err)
	}
	question := messages[0]

	auth.since()
	if _, err = e.A2A().Send(as(ctx, bob), a2a.SendParams{Sender: a2a.Address{Agent: "worker"}, Receivers: []a2a.Address{{Agent: "planner"}}, Performative: a2a.Inform, Content: "answer", ConversationID: question.ConversationID, InReplyTo: question.ReplyWith}); err != nil {
		t.Fatalf("Send reply: %v", err)
	}
	done, err := st.GetRun(ctx, current.ID)
	if err != nil || done.State != run.StateCompleted {
		t.Fatalf("run after the reply = %+v, %v; want completed", done, err)
	}
	visible, authorize := auth.snapshot()
	requireAll(t, "Visible", visible, alice)
	requireAll(t, "Authorize", authorize, alice)
	requireAll(t, "probe handler", handlers.of("probe"), alice)
}

// RecoverAgentReply finishes an admitted reply after a crash, from a
// fresh engine and under the host's recovery context. It restores the
// run's principal before it starts the resume, like every other claim.
func TestAgentReplyRecoveryKeepsOriginalPrincipal(t *testing.T) {
	codec := cortex.JSONPrincipalCodec[testPrincipal]()
	authorize := func(context.Context, id.AgentRunID) error { return nil }

	t.Run("restored", func(t *testing.T) {
		handlers := &handlerPrincipals{}
		// The tool is registered on both engines: the run's frozen
		// allowlist comes from the one that started it.
		st, ctx, current, pauseID := admittedReplyFixtureAs(t, "before_start", alice, WithPrincipalCodec(codec), handlers.tool("probe"))
		auth := &principalRecorder{}
		fresh, err := New(WithStore(st), WithLLM(&sequenceLLM{responses: []llm.Response{callsTool("call-2", "probe")}}),
			WithA2A(a2a.Options{HopCeiling: 4, Workers: 1}), WithToolAuthorizer(auth), WithPrincipalCodec(codec), handlers.tool("probe"))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		recovered, err := fresh.RecoverAgentReply(as(ctx, bob), current.ID, pauseID, authorize)
		if err != nil {
			t.Fatalf("RecoverAgentReply: %v", err)
		}
		if recovered.State != run.StateCompleted {
			t.Fatalf("recovered run state = %q, want completed", recovered.State)
		}
		visible, authorized := auth.snapshot()
		requireAll(t, "Visible", visible, alice)
		requireAll(t, "Authorize", authorized, alice)
		requireAll(t, "probe handler", handlers.of("probe"), alice)
	})

	t.Run("revoked", func(t *testing.T) {
		st, ctx, current, pauseID := admittedReplyFixtureAs(t, "before_start", alice, WithPrincipalCodec(codec))
		revoking := newSwitchCodec()
		revoking.failDecode(fmt.Errorf("member removed: %w", cortex.ErrPrincipalRevoked))
		model := &sequenceLLM{}
		fresh, err := New(WithStore(st), WithLLM(model), WithA2A(a2a.Options{HopCeiling: 4, Workers: 1}),
			WithToolAuthorizer(&principalRecorder{}), WithPrincipalCodec(revoking))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if _, err = fresh.RecoverAgentReply(as(ctx, bob), current.ID, pauseID, authorize); !errors.Is(err, cortex.ErrPrincipalRevoked) {
			t.Fatalf("RecoverAgentReply with a revoked principal = %v, want ErrPrincipalRevoked", err)
		}
		failed, err := st.GetRun(ctx, current.ID)
		if err != nil || failed.State != run.StateFailed {
			t.Fatalf("run after a revoked recovery = %+v, %v; want failed", failed, err)
		}
		if model.callCount() != 0 {
			t.Errorf("the model was called %d times under a revoked principal", model.callCount())
		}
	})
}

// A run that pauses again after a resume keeps its caller through the
// second pause too, so the second resume is as much alice's as the first.
func TestSecondPauseKeepsOriginalPrincipal(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := principalStore(ctx, t)
	auth := &principalRecorder{}
	handlers := &handlerPrincipals{}
	e, err := New(WithStore(st), WithToolAuthorizer(auth), WithPrincipalCodec(cortex.JSONPrincipalCodec[testPrincipal]()),
		WithLLM(&sequenceLLM{responses: []llm.Response{callsTool("call-1", externalTool().Name), callsTool("call-2", externalTool().Name), callsTool("call-3", "probe")}}),
		WithExternalTool(externalTool()), handlers.tool("probe"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	paused, err := e.RunAgent(as(ctx, alice), "steady", "ask twice", nil)
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("RunAgent = %+v, %v; want a paused run", paused, err)
	}
	again, err := e.Resume(as(ctx, bob), paused.ID, ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-1", Content: "yes"}}})
	if err != nil || again.State != run.StatePaused {
		t.Fatalf("first Resume = %+v, %v; want the run paused a second time", again, err)
	}
	stored, err := st.GetRun(ctx, paused.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if len(stored.Principal) == 0 {
		t.Fatal("the second pause dropped the run's principal bytes")
	}

	auth.since()
	done, err := e.Resume(as(ctx, carol), paused.ID, ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-2", Content: "still yes"}}})
	if err != nil || done.State != run.StateCompleted {
		t.Fatalf("second Resume = %+v, %v; want completed", done, err)
	}
	visible, authorize := auth.snapshot()
	requireAll(t, "Visible", visible, alice)
	requireAll(t, "Authorize", authorize, alice)
	requireAll(t, "probe handler", handlers.of("probe"), alice)
}

// A decode that fails for now during an approval records the decision and
// leaves the run ready: nothing ran, and ContinueRun finishes the run as
// its caller once the decoder recovers.
func TestTransientPrincipalErrorDuringApprovalLeavesRunReady(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := principalStore(ctx, t)
	codec := newSwitchCodec()
	handlers := &handlerPrincipals{}
	e, err := New(WithStore(st), WithToolAuthorizer(&principalRecorder{gated: "deploy"}), WithPrincipalCodec(codec),
		WithLLM(&sequenceLLM{responses: []llm.Response{callsTool("call-1", "deploy")}}), handlers.tool("deploy"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	paused, err := e.RunAgent(as(ctx, alice), "steady", "deploy it", nil)
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("RunAgent = %+v, %v; want a paused run", paused, err)
	}
	pending, err := e.ListPendingCheckpoints(ctx, &checkpoint.ListFilter{Limit: 10})
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPendingCheckpoints = %d rows, %v; want 1", len(pending), err)
	}
	decision := checkpoint.Decision{Approved: true, DecidedBy: "bob"}

	codec.failDecode(errors.New("db down"))
	err = e.ResolveCheckpoint(as(ctx, bob), pending[0].ID, decision)
	if !errors.Is(err, cortex.ErrPrincipalUnavailable) || errors.Is(err, cortex.ErrPrincipalRevoked) {
		t.Fatalf("ResolveCheckpoint with a transient decode error = %v, want ErrPrincipalUnavailable and not revoked", err)
	}
	still, err := st.GetRun(ctx, paused.ID)
	if err != nil || still.State != run.StateReady {
		t.Fatalf("run after a transient decode error = %+v, %v; want ready", still, err)
	}
	cp, err := e.GetCheckpoint(ctx, pending[0].ID)
	if err != nil || cp.State != "resolved" || cp.Decision == nil || !cp.Decision.Approved {
		t.Fatalf("checkpoint after a transient decode error = %+v, %v; want the approval recorded", cp, err)
	}
	if got := handlers.of("deploy"); len(got) != 0 {
		t.Fatalf("deploy ran %d times while the principal could not be decoded", len(got))
	}
	if err = e.ResolveCheckpoint(as(ctx, bob), pending[0].ID, decision); !errors.Is(err, cortex.ErrInvalidState) {
		t.Fatalf("deciding again = %v, want ErrInvalidState", err)
	}

	codec.failDecode(nil)
	if _, err = e.ContinueRun(as(ctx, bob), paused.ID); err != nil {
		t.Fatalf("ContinueRun after the decoder recovered: %v", err)
	}
	requireAll(t, "deploy handler", handlers.of("deploy"), alice)
}
