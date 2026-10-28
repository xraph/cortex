package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/suspension"
)

// ttlAuthorizer answers each named tool with a fixed error and allows
// everything else, so a test decides exactly what each call escalates
// with.
type ttlAuthorizer map[string]error

func (a ttlAuthorizer) Visible(_ context.Context, _ cortex.Subject, tools []llm.Tool) []llm.Tool {
	return tools
}

func (a ttlAuthorizer) Authorize(_ context.Context, _ cortex.Subject, call llm.ToolCall) error {
	return a[call.Name]
}

// ttlTool is a registered tool, so approving it would have something to
// run.
func ttlTool(name string) Option {
	return WithTool(llm.Tool{Name: name, Description: "test-only gated tool"}, func(_ context.Context, _ cortex.Invocation) (string, error) {
		return "done", nil
	})
}

// ttlRun drives one run on a real SQLite store whose model asks for calls
// in a single step, and hands back the run with the store and the context
// it ran under.
func ttlRun(t *testing.T, calls []llm.ToolCall, opts ...Option) (*run.Run, store.Store, context.Context) {
	t.Helper()
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := newApprovalStore(ctx, t)
	e, err := New(append([]Option{WithStore(st), WithLLM(&scriptedLLM{toolCalls: calls})}, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, err := e.RunAgent(ctx, "assistant", "do the thing", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	return r, st, ctx
}

// pausedSuspension reads back the one suspension a paused run left.
func pausedSuspension(ctx context.Context, t *testing.T, st store.Store, r *run.Run) *suspension.Suspension {
	t.Helper()
	if r.State != run.StatePaused {
		t.Fatalf("run state = %q, want %q", r.State, run.StatePaused)
	}
	susp, err := st.GetSuspension(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetSuspension: %v", err)
	}
	return susp
}

// requireDeadlineNear checks a deadline lands within a minute of now+want.
func requireDeadlineNear(t *testing.T, got *time.Time, want time.Duration) {
	t.Helper()
	if got == nil {
		t.Fatalf("ExpiresAt = nil, want about now+%s", want)
	}
	if diff := got.Sub(time.Now().Add(want)); diff > time.Minute || diff < -time.Minute {
		t.Fatalf("ExpiresAt is %s from now, want about %s", time.Until(*got).Round(time.Second), want)
	}
}

func TestApprovalTTLSetsSuspensionDeadline(t *testing.T) {
	r, st, ctx := ttlRun(t,
		[]llm.ToolCall{{ID: "call-1", Name: "deploy", Arguments: `{"env":"prod"}`}},
		ttlTool("deploy"),
		WithSuspensionTTL(24*time.Hour),
		WithToolAuthorizer(ttlAuthorizer{"deploy": cortex.ApprovalRequired{Reason: "prod deploy", TTL: 168 * time.Hour}}),
	)
	susp := pausedSuspension(ctx, t, st, r)
	requireDeadlineNear(t, susp.ExpiresAt, 168*time.Hour)
	if len(susp.Pending) != 1 || susp.Pending[0].Note != "prod deploy" {
		t.Fatalf("pending = %+v, want one call noted %q", susp.Pending, "prod deploy")
	}

	pending, err := st.ListPending(ctx, &checkpoint.ListFilter{RunID: r.ID.String(), Limit: 10})
	if err != nil {
		t.Fatalf("ListPending: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("%d pending checkpoints, want 1", len(pending))
	}
	if !strings.Contains(pending[0].Reason, "prod deploy") {
		t.Errorf("checkpoint reason = %q, want it to say why the call needs approval", pending[0].Reason)
	}
}

func TestApprovalWithoutTTLUsesEngineDefault(t *testing.T) {
	r, st, ctx := ttlRun(t,
		[]llm.ToolCall{{ID: "call-1", Name: "deploy", Arguments: `{}`}},
		ttlTool("deploy"),
		WithSuspensionTTL(24*time.Hour),
		WithToolAuthorizer(ttlAuthorizer{"deploy": cortex.ErrRequiresApproval}),
	)
	susp := pausedSuspension(ctx, t, st, r)
	requireDeadlineNear(t, susp.ExpiresAt, 24*time.Hour)
	if susp.Pending[0].Note != "" {
		t.Errorf("a bare sentinel left the note %q, want none", susp.Pending[0].Note)
	}
}

func TestShortestTTLWinsWithinStep(t *testing.T) {
	r, st, ctx := ttlRun(t,
		[]llm.ToolCall{
			{ID: "call-1", Name: "deploy", Arguments: `{}`},
			{ID: "call-2", Name: "rotate", Arguments: `{}`},
		},
		ttlTool("deploy"),
		ttlTool("rotate"),
		WithToolAuthorizer(ttlAuthorizer{
			"deploy": cortex.ApprovalRequired{Reason: "deploy", TTL: 3 * time.Hour},
			"rotate": cortex.ApprovalRequired{Reason: "rotate", TTL: time.Hour},
		}),
	)
	susp := pausedSuspension(ctx, t, st, r)
	if len(susp.Pending) != 2 {
		t.Fatalf("pending = %+v, want both calls", susp.Pending)
	}
	requireDeadlineNear(t, susp.ExpiresAt, time.Hour)
}

// deniedRecorder keeps the reason each ToolDenied carried.
type deniedRecorder struct {
	mu      sync.Mutex
	reasons []string
}

func (d *deniedRecorder) Name() string { return "denied-recorder" }

func (d *deniedRecorder) OnToolDenied(_ context.Context, _ id.AgentRunID, _, reason string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.reasons = append(d.reasons, reason)
	return nil
}

func (d *deniedRecorder) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.reasons)
}

// toolResultLLM is scriptedLLM that also keeps the tool messages the
// model is shown on its second turn.
type toolResultLLM struct {
	scriptedLLM
	mu    sync.Mutex
	shown []string
}

func (l *toolResultLLM) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	l.mu.Lock()
	for _, m := range req.Messages {
		if m.Role == "tool" {
			l.shown = append(l.shown, m.Content)
		}
	}
	l.mu.Unlock()
	return l.scriptedLLM.Complete(ctx, req)
}

func (l *toolResultLLM) results() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.shown)
}

func TestNegativeApprovalTTLDeniesCall(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := newApprovalStore(ctx, t)
	model := &toolResultLLM{scriptedLLM: scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: "deploy", Arguments: `{}`}}}}
	denied := &deniedRecorder{}
	e, err := New(
		WithStore(st),
		WithLLM(model),
		ttlTool("deploy"),
		WithExtension(denied),
		WithToolAuthorizer(ttlAuthorizer{"deploy": cortex.ApprovalRequired{Reason: "prod deploy", TTL: -time.Second}}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r, err := e.RunAgent(ctx, "assistant", "deploy it", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if r.State == run.StatePaused {
		t.Fatal("a call with a negative approval TTL paused the run; it should have been denied")
	}
	if _, getErr := st.GetSuspension(ctx, r.ID); getErr == nil {
		t.Error("a suspension was written for a denied call")
	}

	reasons := denied.snapshot()
	if len(reasons) != 1 || !strings.Contains(reasons[0], "approval TTL is negative") {
		t.Fatalf("ToolDenied reasons = %q, want one saying the approval TTL is negative", reasons)
	}
	shown := model.results()
	if len(shown) != 1 || !strings.Contains(shown[0], "approval TTL is negative") {
		t.Fatalf("the model was shown %q, want the denial", shown)
	}
}

func TestExternalToolTTL(t *testing.T) {
	calls := []llm.ToolCall{{ID: "call-1", Name: "ask_human", Arguments: `{}`}}

	t.Run("ExternalTTL sets the deadline", func(t *testing.T) {
		r, st, ctx := ttlRun(t, calls, WithSuspensionTTL(0), WithExternalTool(externalTool(), ExternalTTL(24*time.Hour)))
		requireDeadlineNear(t, pausedSuspension(ctx, t, st, r).ExpiresAt, 24*time.Hour)
	})

	t.Run("without one, TTL zero leaves no deadline", func(t *testing.T) {
		r, st, ctx := ttlRun(t, calls, WithSuspensionTTL(0), WithExternalTool(externalTool()))
		if got := pausedSuspension(ctx, t, st, r).ExpiresAt; got != nil {
			t.Fatalf("ExpiresAt = %v, want nil", got)
		}
	})
}

// a2aTTLRun drives a planner run on st that asks the worker one
// question, with messaging on, and hands back the paused run.
func a2aTTLRun(ctx context.Context, t *testing.T, st store.Store, opts a2a.Options, engineOpts ...Option) *run.Run {
	t.Helper()
	base := []Option{
		WithStore(st),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"status?"}`}}}),
		WithA2A(opts),
	}
	e, err := New(append(base, engineOpts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range []string{"planner", "worker"} {
		if createErr := e.CreateAgent(ctx, &agent.Config{ID: id.NewAgentID(), Name: name, SystemPrompt: "you are " + name, Model: "test-model", MaxSteps: 4}); createErr != nil {
			t.Fatalf("CreateAgent %s: %v", name, createErr)
		}
	}
	r, err := e.RunAgent(ctx, "planner", "ask the worker", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	return r
}

func TestAgentReplyDeadlineFollowsAskReplyBy(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := newApprovalStore(ctx, t)
	r := a2aTTLRun(ctx, t, st,
		a2a.Options{HopCeiling: 4, Workers: 1, DefaultReplyBy: 2 * time.Hour},
		WithSuspensionTTL(time.Hour),
		WithAgentReplyGrace(5*time.Minute),
	)
	susp := pausedSuspension(ctx, t, st, r)
	if susp.Reason != suspension.ReasonAgentReply {
		t.Fatalf("reason = %q, want %q", susp.Reason, suspension.ReasonAgentReply)
	}
	requireDeadlineNear(t, susp.ExpiresAt, 2*time.Hour+5*time.Minute)
}

// suspendedRecorder keeps every Notice and, for each, the run state the
// store showed from inside the hook.
type suspendedRecorder struct {
	st store.Store

	mu      sync.Mutex
	notices []suspension.Notice
	states  []run.State
	created int
}

func (s *suspendedRecorder) Name() string { return "suspended-recorder" }

func (s *suspendedRecorder) OnRunSuspended(ctx context.Context, _ id.AgentID, runID id.AgentRunID, n suspension.Notice) error {
	var state run.State
	if r, err := s.st.GetRun(ctx, runID); err == nil {
		state = r.State
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notices = append(s.notices, n)
	s.states = append(s.states, state)
	return nil
}

func (s *suspendedRecorder) OnCheckpointCreated(_ context.Context, _ id.CheckpointID, _ id.AgentRunID, _ string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.created++
	return nil
}

func (s *suspendedRecorder) snapshot() ([]suspension.Notice, []run.State, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.notices), slices.Clone(s.states), s.created
}

func TestSweeperHonoursPerSuspensionDeadline(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := newApprovalStore(ctx, t)
	hook := &suspendedRecorder{st: st}
	e, err := New(
		WithStore(st),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: "deploy", Arguments: `{}`}}}),
		ttlTool("deploy"),
		WithExtension(hook),
		WithSuspensionTTL(0),
		WithSuspensionSweepInterval(10*time.Millisecond),
		WithToolAuthorizer(ttlAuthorizer{"deploy": cortex.ApprovalRequired{Reason: "quick look", TTL: 50 * time.Millisecond}}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if startErr := e.Start(context.Background()); startErr != nil {
		t.Fatalf("Start: %v", startErr)
	}
	t.Cleanup(func() { _ = e.Stop(context.Background()) })

	r, err := e.RunAgent(ctx, "assistant", "deploy it", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if r.State != run.StatePaused {
		t.Fatalf("run state = %q, want %q", r.State, run.StatePaused)
	}

	var final *run.Run
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, getErr := st.GetRun(ctx, r.ID)
		if getErr != nil {
			t.Fatalf("GetRun: %v", getErr)
		}
		if got.State == run.StateFailed {
			final = got
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run is still %q two seconds after a 50ms deadline; the sweeper ignored it", got.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(final.Error, cortex.ErrSuspensionExpired.Error()) {
		t.Errorf("run error = %q, want it to wrap %q", final.Error, cortex.ErrSuspensionExpired)
	}

	notices, _, _ := hook.snapshot()
	if len(notices) != 1 || notices[0].CheckpointID.IsNil() {
		t.Fatalf("notices = %+v, want one carrying the checkpoint", notices)
	}
	// The sweep fails the run first and resolves the checkpoint after, so
	// the checkpoint is waited for on its own.
	for {
		cp, getErr := st.GetCheckpoint(ctx, notices[0].CheckpointID)
		if getErr != nil {
			t.Fatalf("GetCheckpoint: %v", getErr)
		}
		if cp.Decision != nil {
			if cp.Decision.DecidedBy != checkpointDecidedByEngine {
				t.Fatalf("checkpoint decided by %q, want %q", cp.Decision.DecidedBy, checkpointDecidedByEngine)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the expired run's checkpoint was never resolved")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestRunSuspendedFiresForEveryReason(t *testing.T) {
	cases := []struct {
		reason suspension.SuspendReason
		drive  func(t *testing.T, hook *suspendedRecorder) (*run.Run, store.Store, context.Context)
	}{
		{
			reason: suspension.ReasonApproval,
			drive: func(t *testing.T, hook *suspendedRecorder) (*run.Run, store.Store, context.Context) {
				ctx := cortex.WithScope(context.Background(), approvalScope())
				st := newApprovalStore(ctx, t)
				hook.st = st
				e, err := New(WithStore(st), WithExtension(hook), ttlTool("deploy"),
					WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: "deploy", Arguments: `{}`}}}),
					WithToolAuthorizer(ttlAuthorizer{"deploy": cortex.ApprovalRequired{Reason: "prod deploy", TTL: 2 * time.Hour}}))
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				r, err := e.RunAgent(ctx, "assistant", "deploy it", nil)
				if err != nil {
					t.Fatalf("RunAgent: %v", err)
				}
				return r, st, ctx
			},
		},
		{
			reason: suspension.ReasonExternalTool,
			drive: func(t *testing.T, hook *suspendedRecorder) (*run.Run, store.Store, context.Context) {
				ctx := cortex.WithScope(context.Background(), approvalScope())
				st := newApprovalStore(ctx, t)
				hook.st = st
				e, err := New(WithStore(st), WithExtension(hook), WithExternalTool(externalTool()),
					WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: "ask_human", Arguments: `{}`}}}))
				if err != nil {
					t.Fatalf("New: %v", err)
				}
				r, err := e.RunAgent(ctx, "assistant", "ask", nil)
				if err != nil {
					t.Fatalf("RunAgent: %v", err)
				}
				return r, st, ctx
			},
		},
		{
			reason: suspension.ReasonAgentReply,
			drive: func(t *testing.T, hook *suspendedRecorder) (*run.Run, store.Store, context.Context) {
				ctx := cortex.WithScope(context.Background(), approvalScope())
				st := newApprovalStore(ctx, t)
				hook.st = st
				return a2aTTLRun(ctx, t, st, a2a.Options{HopCeiling: 4, Workers: 1}, WithExtension(hook)), st, ctx
			},
		},
	}

	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			hook := &suspendedRecorder{}
			r, st, ctx := tc.drive(t, hook)
			susp := pausedSuspension(ctx, t, st, r)

			notices, states, _ := hook.snapshot()
			if len(notices) != 1 {
				t.Fatalf("OnRunSuspended fired %d times, want 1", len(notices))
			}
			n := notices[0]
			if n.Reason != tc.reason || n.Reason != susp.Reason {
				t.Errorf("notice reason = %q, want %q (stored %q)", n.Reason, tc.reason, susp.Reason)
			}
			if n.SuspensionID != susp.ID {
				t.Errorf("notice suspension = %s, want %s", n.SuspensionID, susp.ID)
			}
			if !slices.Equal(pendingNames(n.Pending), pendingNames(susp.Pending)) {
				t.Errorf("notice pending = %v, want %v", pendingNames(n.Pending), pendingNames(susp.Pending))
			}
			if (n.ExpiresAt == nil) != (susp.ExpiresAt == nil) || (n.ExpiresAt != nil && !n.ExpiresAt.Equal(*susp.ExpiresAt)) {
				t.Errorf("notice ExpiresAt = %v, want the stored %v", n.ExpiresAt, susp.ExpiresAt)
			}
			if got := n.CheckpointID.IsNil(); got == (tc.reason == suspension.ReasonApproval) {
				t.Errorf("notice checkpoint = %q; it is set for an approval and only for one", n.CheckpointID)
			}
			if n.Scope.Canonical() != approvalScope().Canonical() {
				t.Errorf("notice scope = %v, want the run's", n.Scope)
			}
			if states[0] != run.StatePaused {
				t.Errorf("inside the hook the run was %q, want %q", states[0], run.StatePaused)
			}
		})
	}
}

func pendingNames(calls []suspension.PendingCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.Name)
	}
	return out
}

// pauseFailStore refuses the flip to paused, and nothing else.
type pauseFailStore struct {
	*sqlitestore.Store
}

func (p pauseFailStore) UpdateRun(ctx context.Context, r *run.Run) error {
	if r.State == run.StatePaused {
		return errors.New("injected: cannot pause")
	}
	return p.Store.UpdateRun(ctx, r)
}

func TestRunSuspendedNotFiredWhenPauseFails(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := pauseFailStore{newApprovalStore(ctx, t)}
	hook := &suspendedRecorder{st: st}
	e, err := New(WithStore(st), WithExtension(hook), ttlTool("deploy"),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: "deploy", Arguments: `{}`}}}),
		WithToolAuthorizer(ttlAuthorizer{"deploy": cortex.ApprovalRequired{Reason: "prod deploy"}}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := e.RunAgent(ctx, "assistant", "deploy it", nil); err == nil {
		t.Fatal("RunAgent returned no error though the run could not be paused")
	}
	notices, _, created := hook.snapshot()
	if len(notices) != 0 {
		t.Errorf("OnRunSuspended fired %d times for a run that never paused", len(notices))
	}
	if created != 0 {
		t.Errorf("OnCheckpointCreated fired %d times for a run that never paused", created)
	}
}

// A negative per-call deadline reads as one already passed, so the
// options refuse it the way WithSuspensionTTL does.
func TestNegativeExternalTTLAndReplyGraceAreRefused(t *testing.T) {
	if _, err := New(WithExternalTool(externalTool(), ExternalTTL(-time.Hour))); err == nil {
		t.Error("New accepted a negative ExternalTTL")
	}
	if _, err := New(WithAgentReplyGrace(-time.Minute)); err == nil {
		t.Error("New accepted a negative agent reply grace")
	}
}
