package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
	"github.com/xraph/cortex/suspension"
)

// externalTool is the definition every test here registers with
// WithExternalTool.
func externalTool() llm.Tool {
	return llm.Tool{Name: "ask_human", Description: "test-only external tool"}
}

// scriptedLLM answers its first Complete with a fixed set of tool calls
// and every later one with plain content, so a single run dispatches
// exactly the calls a test wants and then finishes. scopespy's
// ToolCallingLLM only ever requests one call, and the suspend rule under
// test here is about a step carrying several.
type scriptedLLM struct {
	mu        sync.Mutex
	toolCalls []llm.ToolCall
	answered  bool
}

func (s *scriptedLLM) Complete(_ context.Context, _ *llm.Request) (*llm.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.answered {
		s.answered = true
		return &llm.Response{ToolCalls: s.toolCalls}, nil
	}
	return &llm.Response{Content: "done"}, nil
}

func (s *scriptedLLM) CompleteStream(_ context.Context, _ *llm.Request) (llm.Stream, error) {
	return nil, errors.New("scriptedLLM: CompleteStream not supported")
}

// suspendingRun drives one run whose model calls the external tool once,
// and returns the run alongside the store that recorded the suspension.
func suspendingRun(t *testing.T, opts ...Option) (*run.Run, *scopespy.Spy) {
	t.Helper()

	spy := scopespy.New()
	def := externalTool()
	base := []Option{
		WithStore(spy),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{
			{ID: "call-1", Name: def.Name, Arguments: `{"question":"proceed?"}`},
		}}),
		WithExternalTool(def),
	}
	e, err := New(append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	r, err := e.RunAgent(ctx, "assistant", "ask the human", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	return r, spy
}

func TestWithExternalTool_AdvertisedInResolveTools(t *testing.T) {
	def := externalTool()
	e, err := New(WithExternalTool(def))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := e.resolveTools(context.Background(), cortex.Subject{}, nil)
	if len(got) != 1 || got[0].Name != def.Name {
		t.Fatalf("resolveTools = %v, want the external tool %q; a tool the model is never shown can never be called", got, def.Name)
	}
}

// TestResolveTools_ExternalToolsObeyCfgToolsNames pins which side of the
// v1.10.0 filtering rule external tools landed on. They are host
// registrations like WithTool's, so cfg.Tools selects among them; they
// are not builtins, which are exempt because they exist as a consequence
// of engine config the agent never named.
func TestResolveTools_ExternalToolsObeyCfgToolsNames(t *testing.T) {
	def := externalTool()
	registered := llm.Tool{Name: "echo_back", Description: "a registered tool"}

	tests := []struct {
		name  string
		names []string
		want  bool
	}{
		{name: "named", names: []string{def.Name}, want: true},
		{name: "not named", names: []string{registered.Name}, want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e, err := New(
				WithExternalTool(def),
				WithTool(registered, func(_ context.Context, _ cortex.Invocation) (string, error) {
					return "ok", nil
				}),
			)
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			var found bool
			for _, tl := range e.resolveTools(context.Background(), cortex.Subject{}, tc.names) {
				if tl.Name == def.Name {
					found = true
				}
			}
			if found != tc.want {
				t.Errorf("resolveTools with cfg.Tools=%v advertised the external tool = %t, want %t", tc.names, found, tc.want)
			}
		})
	}
}

// TestResolveTools_ExternalToolsGoThroughVisible keeps external tools on
// the same security boundary as everything else: an authorizer that hides
// them must actually hide them.
func TestResolveTools_ExternalToolsGoThroughVisible(t *testing.T) {
	e, err := New(
		WithExternalTool(externalTool()),
		WithToolAuthorizer(&recordingAuthorizer{visible: nil}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if got := e.resolveTools(context.Background(), cortex.Subject{}, nil); len(got) != 0 {
		t.Fatalf("resolveTools = %v, want nothing; the authorizer hid every tool", got)
	}
}

func TestExecuteTool_ExternalToolIsPending(t *testing.T) {
	def := externalTool()
	e, err := New(WithExternalTool(def))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, outcome, _ := e.executeTool(context.Background(), cortex.Subject{}, llm.ToolCall{Name: def.Name})
	if outcome != outcomePending {
		t.Errorf("outcome = %d, want outcomePending (%d)", outcome, outcomePending)
	}
	if result != "" {
		t.Errorf("executeTool returned %q for a pending call; nothing ran, so there is no result to give the model", result)
	}
}

// TestExecuteTool_DeniedExternalToolStaysDenied pins the order of the two
// gates. A denial is a terminal outcome the model gets told about; if the
// external check ran first, denying a call to an external tool would
// suspend the run instead of refusing it, and the host would be asked to
// execute a call its own authorizer just rejected.
func TestExecuteTool_DeniedExternalToolStaysDenied(t *testing.T) {
	def := externalTool()
	rec := &toolEventRecorder{}
	e, err := New(
		WithExternalTool(def),
		WithToolAuthorizer(&recordingAuthorizer{authorizeErr: errors.New("not permitted")}),
		WithExtension(rec),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, outcome, _ := e.executeTool(context.Background(), cortex.Subject{}, llm.ToolCall{Name: def.Name})
	if outcome != outcomeDenied {
		t.Errorf("outcome = %d, want outcomeDenied (%d)", outcome, outcomeDenied)
	}
	if got := countEvent(rec.snapshot(), "denied"); got != 1 {
		t.Errorf("ToolDenied fired %d times, want 1: %v", got, rec.snapshot())
	}
}

// TestDispatch_ExternalToolIsNotDispatchable covers the one caller of
// executeTool that has no run to suspend. It must say so rather than pass
// the empty pending result off as a tool result.
func TestDispatch_ExternalToolIsNotDispatchable(t *testing.T) {
	def := externalTool()
	e, err := New(WithExternalTool(def))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := e.Dispatch(context.Background(), def.Name, "{}")
	if got == "" {
		t.Fatal("Dispatch returned an empty string for an external tool; that reads as a tool that ran and said nothing")
	}
	// Asserting non-empty alone proves nothing: deleting the external
	// arm from executeTool entirely yields the unknown-tool payload,
	// which is also non-empty. The message has to name the contract.
	if !strings.Contains(got, "external") {
		t.Errorf("Dispatch = %q, want it to say the tool is external and answered by suspending a run", got)
	}
	if strings.Contains(got, "unknown tool") {
		t.Errorf("Dispatch = %q, but the engine knows this tool; it is external, not unknown", got)
	}
}

// TestRunAgent_SuspendsOnExternalToolCall proves the point of external
// tool suspension: run.StatePaused has existed since v1.x and has never
// been assigned until here.
func TestRunAgent_SuspendsOnExternalToolCall(t *testing.T) {
	r, spy := suspendingRun(t)

	if r.State != run.StatePaused {
		t.Fatalf("run state = %q, want %q", r.State, run.StatePaused)
	}

	susps := spy.Suspensions()
	if len(susps) != 1 {
		t.Fatalf("the run wrote %d suspensions, want exactly 1", len(susps))
	}
	s := susps[0]
	if s.RunID != r.ID {
		t.Errorf("suspension RunID = %q, want the run's own id %q", s.RunID, r.ID)
	}
	if s.Reason != suspension.ReasonExternalTool {
		t.Errorf("suspension reason = %q, want %q", s.Reason, suspension.ReasonExternalTool)
	}
	if s.Scope.IsZero() {
		t.Error("suspension carries a zero scope; Resume re-derives authorization from it")
	}

	if len(s.Pending) != 1 {
		t.Fatalf("suspension carries %d pending calls, want 1: %+v", len(s.Pending), s.Pending)
	}
	p := s.Pending[0]
	if p.ID != "call-1" || p.Name != externalTool().Name {
		t.Errorf("pending call = %+v, want the model's own call id and name", p)
	}
	if p.Arguments != `{"question":"proceed?"}` {
		t.Errorf("pending call arguments = %q, want the model's arguments verbatim; the caller cannot execute the call without them", p.Arguments)
	}
}

// TestRunAgent_SuspensionCarriesTheContinuation covers what a resume
// actually reads back. The pending call must have no tool result message
// waiting for it: the model has not been answered yet.
func TestRunAgent_SuspensionCarriesTheContinuation(t *testing.T) {
	_, spy := suspendingRun(t)

	susps := spy.Suspensions()
	if len(susps) != 1 {
		t.Fatalf("the run wrote %d suspensions, want exactly 1", len(susps))
	}
	cont := susps[0].Cont

	// The run suspended during step 0, and stepIndex had already advanced
	// past it, so a resume picks up at step 1 with the rest of the
	// MaxSteps budget intact.
	if cont.StepIndex != 1 {
		t.Errorf("continuation StepIndex = %d, want 1 (the next step to run)", cont.StepIndex)
	}
	if cont.SystemPrompt == "" {
		t.Error("continuation carries no system prompt; the resumed run would answer as a different agent")
	}
	if len(cont.Messages) == 0 {
		t.Fatal("continuation carries no messages")
	}
	last := cont.Messages[len(cont.Messages)-1]
	if last.Role != "assistant" || len(last.ToolCalls) != 1 {
		t.Errorf("continuation's last message = %+v, want the assistant message carrying the tool call", last)
	}
	for _, m := range cont.Messages {
		if m.Role == "tool" {
			t.Errorf("continuation carries a tool result message %+v for a call that never ran", m)
		}
	}
}

// TestRunAgent_SuspendedRunFiresNoTerminalToolEvent holds the invariant
// v1.10.0 established: exactly one of completed, failed and denied per
// call. A pending call has reached none of them, so it fires none.
func TestRunAgent_SuspendedRunFiresNoTerminalToolEvent(t *testing.T) {
	rec := &toolEventRecorder{}
	_, _ = suspendingRun(t, WithExtension(rec))

	events := rec.snapshot()
	if got := countEvent(events, "called"); got != 1 {
		t.Fatalf("ToolCalled fired %d times, want 1; the run did not dispatch one tool call: %v", got, events)
	}
	for _, terminal := range []string{"completed", "failed", "denied"} {
		if got := countEvent(events, terminal); got != 0 {
			t.Errorf("Tool%s fired %d times for a pending call, want 0: %v", terminal, got, events)
		}
	}
}

// TestRunAgent_SuspendsOnceForAWholeStep is why the pending calls are
// collected in the loop instead of each site suspending on its own. Two
// external calls and one executable call arrive in a single step: the
// executable one has to run and be recorded, and the step has to produce
// one suspension carrying both pending calls, not two suspensions or one
// that forgot a call.
func TestRunAgent_SuspendsOnceForAWholeStep(t *testing.T) {
	def := externalTool()
	registered := llm.Tool{Name: "echo_back", Description: "a registered tool"}
	rec := &toolEventRecorder{}
	spy := scopespy.New()

	e, err := New(
		WithStore(spy),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{
			{ID: "call-1", Name: def.Name, Arguments: `{"n":1}`},
			{ID: "call-2", Name: registered.Name, Arguments: `{"n":2}`},
			{ID: "call-3", Name: def.Name, Arguments: `{"n":3}`},
		}}),
		WithExternalTool(def),
		WithTool(registered, func(_ context.Context, _ cortex.Invocation) (string, error) {
			return "handler ran", nil
		}),
		WithExtension(rec),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	r, err := e.RunAgent(ctx, "assistant", "call everything", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	if r.State != run.StatePaused {
		t.Fatalf("run state = %q, want %q", r.State, run.StatePaused)
	}
	susps := spy.Suspensions()
	if len(susps) != 1 {
		t.Fatalf("the step wrote %d suspensions, want exactly 1", len(susps))
	}
	pending := susps[0].Pending
	if len(pending) != 2 {
		t.Fatalf("suspension carries %d pending calls, want 2: %+v", len(pending), pending)
	}
	if pending[0].ID != "call-1" || pending[1].ID != "call-3" {
		t.Errorf("pending calls = %+v, want call-1 and call-3 in the order the model asked for them", pending)
	}
	if got := countEvent(rec.snapshot(), "completed"); got != 1 {
		t.Errorf("ToolCompleted fired %d times, want 1: the executable sibling call still has to run and be reported", got)
	}
	// The executable sibling's result has to survive into the
	// continuation, or resuming would re-ask the model with a tool call
	// it never got an answer for.
	var toolResults int
	for _, m := range susps[0].Cont.Messages {
		if m.Role == "tool" {
			toolResults++
		}
	}
	if toolResults != 1 {
		t.Errorf("continuation carries %d tool result messages, want 1 (the sibling that did run)", toolResults)
	}
}

// runStateSpy records the state every UpdateRun carried. The base Spy
// records that the call happened and discards the run, and the write
// order inside suspend can only be judged by what the run said when it
// was written.
type runStateSpy struct {
	*scopespy.Spy
	mu     sync.Mutex
	states []run.State
}

func (r *runStateSpy) UpdateRun(ctx context.Context, rn *run.Run) error {
	r.mu.Lock()
	r.states = append(r.states, rn.State)
	r.mu.Unlock()
	return r.Spy.UpdateRun(ctx, rn)
}

func (r *runStateSpy) wroteStates() []run.State {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]run.State(nil), r.states...)
}

// TestRunAgent_SuspensionWriteFailureDoesNotPauseTheRun pins the write
// order inside suspend. A run marked paused with no suspension behind it
// can never be resumed: ClaimSuspension only finds a suspension for a
// paused run, and there is nothing to find. So the suspension is written
// first, and a run whose suspension could not be written must never have
// been persisted as paused at all.
func TestRunAgent_SuspensionWriteFailureDoesNotPauseTheRun(t *testing.T) {
	base := scopespy.New()
	base.FailSuspensionWrites(errors.New("store is down"))
	spy := &runStateSpy{Spy: base}
	def := externalTool()

	e, err := New(
		WithStore(spy),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: def.Name, Arguments: "{}"}}}),
		WithExternalTool(def),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	r, err := e.RunAgent(ctx, "assistant", "ask the human", nil)
	if err == nil {
		t.Fatalf("RunAgent returned no error though the suspension could not be written; run = %+v", r)
	}
	if r != nil {
		t.Errorf("RunAgent returned a run alongside the error: %+v", r)
	}
	if len(spy.Suspensions()) != 0 {
		t.Error("a suspension was recorded despite the write failing")
	}
	for _, st := range spy.wroteStates() {
		if st == run.StatePaused {
			t.Errorf("the run was persisted as %q with no suspension behind it; it could never be resumed. States written: %v", run.StatePaused, spy.wroteStates())
		}
	}
}

// TestStreamAgent_SuspendsOnExternalToolCall covers the second copy of
// the tool-dispatch loop. A suspend implemented in only one of them would
// leave streaming hosts dispatching external calls as unknown tools.
func TestStreamAgent_SuspendsOnExternalToolCall(t *testing.T) {
	def := externalTool()
	spy := scopespy.New()
	rec := &toolEventRecorder{}

	e, err := New(
		WithStore(spy),
		WithLLM(scopespy.ToolCallingStreamLLM(def.Name)),
		WithExternalTool(def),
		WithExtension(rec),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	events := make(chan StreamEvent, 64)
	if err := e.StreamAgent(ctx, "assistant", "ask the human", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}

	// streamReAct closes the channel only after its goroutine has
	// finished every emit, so draining is the synchronisation point.
	var sawSuspended, sawDone bool
	for ev := range events {
		switch ev.Type {
		case EventSuspended:
			sawSuspended = true
		case EventDone:
			sawDone = true
		}
	}
	if !sawSuspended {
		t.Error("the stream never emitted EventSuspended; a caller would see the channel close and read the run as finished")
	}
	if sawDone {
		t.Error("the stream emitted EventDone for a run that suspended")
	}

	susps := spy.Suspensions()
	if len(susps) != 1 {
		t.Fatalf("the streaming run wrote %d suspensions, want exactly 1", len(susps))
	}
	if susps[0].Reason != suspension.ReasonExternalTool {
		t.Errorf("suspension reason = %q, want %q", susps[0].Reason, suspension.ReasonExternalTool)
	}
	if got := countEvent(rec.snapshot(), "completed"); got != 0 {
		t.Errorf("ToolCompleted fired %d times on a suspended streaming call, want 0", got)
	}
}

// historySpy returns a fixed conversation history from LoadConversation.
// The base Spy returns none, and the boundary a continuation has to carry
// only exists when there IS a history in front of the run's own messages.
type historySpy struct {
	*scopespy.Spy
	history []memory.Message
}

func (h *historySpy) LoadConversation(ctx context.Context, agentID id.AgentID, sessionID id.SessionID, limit int) ([]memory.Message, error) {
	if _, err := h.Spy.LoadConversation(ctx, agentID, sessionID, limit); err != nil {
		return nil, err
	}
	return h.history, nil
}

// TestRunAgent_ContinuationSeparatesHistoryFromNewMessages is the fix for
// the defect that shipped in v1.8.0. The continuation carries the loaded
// history alongside the run's own messages, so without the boundary a
// resume's first SaveConversation writes the entire history back as new
// rows, and the fixed-size read window fills with duplicates until the
// agent stops seeing recent turns.
//
// The end-to-end proof (suspend, resume, assert no duplicate rows) needs
// Resume to exist first. What is provable here is that the writer put
// the right boundary and the right session in the row Resume will read.
func TestRunAgent_ContinuationSeparatesHistoryFromNewMessages(t *testing.T) {
	const historyLen = 3
	history := make([]memory.Message, historyLen)
	for i := range history {
		history[i] = memory.Message{Role: "user", Content: "earlier turn"}
	}
	spy := &historySpy{Spy: scopespy.New(), history: history}
	def := externalTool()

	e, err := New(
		WithStore(spy),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: def.Name, Arguments: "{}"}}}),
		WithExternalTool(def),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	r, err := e.RunAgent(ctx, "assistant", "ask the human", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	susps := spy.Suspensions()
	if len(susps) != 1 {
		t.Fatalf("the run wrote %d suspensions, want exactly 1", len(susps))
	}
	cont := susps[0].Cont

	if cont.NewMessagesFrom != historyLen {
		t.Errorf("continuation NewMessagesFrom = %d, want %d (the loaded history length); a resume would re-save the history", cont.NewMessagesFrom, historyLen)
	}
	if len(cont.Messages) <= cont.NewMessagesFrom {
		t.Fatalf("continuation carries %d messages with a boundary at %d; the run's own messages are missing", len(cont.Messages), cont.NewMessagesFrom)
	}
	// Everything before the boundary must be the history verbatim, or the
	// index points at the wrong message.
	if cont.Messages[cont.NewMessagesFrom].Content != "ask the human" {
		t.Errorf("the message at the boundary is %q, want the run's own input; the boundary is off", cont.Messages[cont.NewMessagesFrom].Content)
	}

	if cont.SessionID.IsNil() {
		t.Error("continuation carries no session id; a resume cannot save into the session the messages came from")
	}
	if cont.SessionID != r.SessionID {
		t.Errorf("continuation SessionID = %q, want the run's own session %q", cont.SessionID, r.SessionID)
	}
}

// orphanSpy fails the run update that follows a successful suspension
// write, and records every DeleteSuspension the engine issues.
type orphanSpy struct {
	*scopespy.Spy
	mu      sync.Mutex
	deleted []id.AgentRunID
}

func (o *orphanSpy) UpdateRun(ctx context.Context, r *run.Run) error {
	if r.State == run.StatePaused {
		return errors.New("update rejected")
	}
	return o.Spy.UpdateRun(ctx, r)
}

func (o *orphanSpy) DeleteSuspension(_ context.Context, runID id.AgentRunID) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.deleted = append(o.deleted, runID)
	return nil
}

func (o *orphanSpy) deletions() []id.AgentRunID {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]id.AgentRunID(nil), o.deleted...)
}

// TestSuspend_FailedStateFlipDeletesTheSuspension covers the other half
// of the write order. The suspension is written first on purpose, so when
// the flip to paused fails the row is left attached to a run that is
// about to be failed: ExpiresAt is nil, so the sweeper never sees it, and
// nothing else would ever delete it.
func TestSuspend_FailedStateFlipDeletesTheSuspension(t *testing.T) {
	spy := &orphanSpy{Spy: scopespy.New()}
	def := externalTool()

	e, err := New(
		WithStore(spy),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: def.Name, Arguments: "{}"}}}),
		WithExternalTool(def),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	r, err := e.RunAgent(ctx, "assistant", "ask the human", nil)
	if err == nil {
		t.Fatalf("RunAgent returned no error though the run could not be paused; run = %+v", r)
	}

	susps := spy.Suspensions()
	if len(susps) != 1 {
		t.Fatalf("the run wrote %d suspensions, want 1; there is nothing to clean up otherwise", len(susps))
	}
	deleted := spy.deletions()
	if len(deleted) != 1 {
		t.Fatalf("DeleteSuspension called %d times, want 1: the suspension is stranded on a failed run", len(deleted))
	}
	if deleted[0] != susps[0].RunID {
		t.Errorf("DeleteSuspension got run %q, want the suspended run %q", deleted[0], susps[0].RunID)
	}
}
