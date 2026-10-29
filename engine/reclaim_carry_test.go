package engine_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// ownerlessGraceTTL is the lease ttl these tests run under. The sweep
// takes an ownerless run, or closes a stale checkpoint, once it has been
// idle for twice that.
const ownerlessGraceTTL = 40 * time.Millisecond

// pastGrace waits until anything written before it is older than the
// sweep's grace. Nothing sweeps on its own here, so the wait cannot race a
// sweep: each test sweeps when it wants to.
func pastGrace() { time.Sleep(3 * ownerlessGraceTTL) }

func TestSweeperReclaimsOwnerlessRunningRuns(t *testing.T) {
	f := newLeaseFixture(t, newLeaseLLM())
	ag, err := f.store.GetByName(leaseScope(), "assistant")
	require.NoError(t, err)
	create := func(provenance run.ExecutionProvenance, lease *run.Lease) *run.Run {
		r := &run.Run{Entity: cortex.NewEntity(), ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StateRunning, Input: "work", Provenance: provenance, Lease: lease}
		require.NoError(t, f.store.CreateRun(leaseScope(), r))
		return r
	}
	ownerless := create(run.ExecutionProvenance{}, nil)
	claimed := create(run.ExecutionProvenance{TaskClaimID: "claim-1"}, nil)
	verifying := create(run.ExecutionProvenance{VerificationRequired: true}, nil)
	live := create(run.ExecutionProvenance{}, &run.Lease{Owner: "alive", ExpiresAt: time.Now().UTC().Add(time.Hour)})

	// Inside the grace (an hour-long lease here) the run may be between its
	// claim and its lease, and it is left alone.
	patient := reclaimer(t, f, &reclaimLLM{}, newReclaimHooks(), engine.WithRunLease(time.Hour), engine.WithWorkerID("patient"))
	engine.SweepOnce(patient, context.Background())
	got, err := f.store.GetRun(leaseScope(), ownerless.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, got.State, "a run inside the grace may be between its claim and its lease")

	hooks := newReclaimHooks()
	b := reclaimer(t, f, &reclaimLLM{}, hooks, engine.WithRunLease(ownerlessGraceTTL))
	pastGrace()
	// An engine with leases off never takes a run for having no owner.
	unleased := reclaimer(t, f, &reclaimLLM{}, newReclaimHooks(), engine.WithRunLease(0), engine.WithWorkerID("unleased"))
	engine.SweepOnce(unleased, context.Background())
	got, err = f.store.GetRun(leaseScope(), ownerless.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, got.State)

	engine.SweepOnce(b, context.Background())
	got, err = f.store.GetRun(leaseScope(), ownerless.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateReclaimable, got.State)
	require.Equal(t, 1, got.Reclaims)
	seen := hooks.snapshot()
	require.Len(t, seen.reclaimable, 1)
	require.Empty(t, seen.reclaimable[0].PreviousOwner)
	require.Equal(t, 1, seen.reclaimable[0].Reclaims)
	for _, untouched := range []*run.Run{claimed, verifying, live} {
		stored, readErr := f.store.GetRun(leaseScope(), untouched.ID)
		require.NoError(t, readErr)
		require.Equal(t, run.StateRunning, stored.State, "run %s was taken", untouched.ID)
	}
}

// A worker that claimed a run from its pause and died before it took the
// lease leaves the run running with no owner and no progress. The sweep
// reclaims it, and ContinueRun continues from the pause it was claimed
// from: the external call is pending again, so the run pauses again.
func TestReclaimContinuesFromTheClaimedPause(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-x", Name: "lookup", Arguments: `{"q":1}`}))
	f := newLeaseFixture(t, model, engine.WithExternalTool(llm.Tool{Name: "lookup"}))
	paused, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, paused.State)
	before, err := f.store.GetSuspension(leaseScope(), paused.ID)
	require.NoError(t, err)
	// The resumer's claim, and nothing after it.
	_, err = f.store.ClaimSuspension(leaseScope(), paused.ID)
	require.NoError(t, err)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	b := reclaimer(t, f, bModel, hooks, engine.WithRunLease(ownerlessGraceTTL), engine.WithExternalTool(llm.Tool{Name: "lookup"}))
	pastGrace()
	engine.SweepOnce(b, context.Background())
	require.Len(t, hooks.snapshot().reclaimable, 1)

	again, err := b.ContinueRun(leaseScope(), paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, again.State)
	require.Zero(t, bModel.calls(), "the model was called before the pending call was answered")
	after, err := f.store.GetSuspension(leaseScope(), paused.ID)
	require.NoError(t, err)
	require.NotEqual(t, before.ID, after.ID, "the run must pause on a new suspension")
	require.Equal(t, suspension.ReasonExternalTool, after.Reason)
	require.Equal(t, []string{"call-x"}, startedIDs(after.Pending))
	seen := hooks.snapshot()
	require.Len(t, seen.suspended, 1)
	require.Equal(t, after.ID, seen.suspended[0].SuspensionID)
}

// mustStoredPause writes a paused run with an external-tool suspension
// and a loop checkpoint bound to it, straight through the store.
func mustStoredPause(t *testing.T, f *leaseFixture) (*run.Run, *suspension.Suspension, *checkpoint.Checkpoint) {
	t.Helper()
	r := storedRunningRun(t, f, run.Lease{Owner: "gone", ExpiresAt: time.Now().UTC().Add(time.Hour)})
	susp := &suspension.Suspension{Entity: cortex.NewEntity(), ID: id.NewSuspensionID(), RunID: r.ID, Reason: suspension.ReasonApproval,
		Pending: []suspension.PendingCall{{ID: "call-x", Name: "x", Arguments: "{}"}},
		Cont: suspension.Continuation{Config: suspension.RunConfig{MaxSteps: 3}, StepIndex: 1, Messages: []llm.Message{
			{Role: "user", Content: "work"},
			{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "call-x", Name: "x", Arguments: "{}"}}},
		}}}
	require.NoError(t, f.store.CreateSuspension(leaseScope(), susp))
	cp := &checkpoint.Checkpoint{ID: id.NewCheckpointID(), RunID: r.ID, AgentID: r.AgentID, Reason: "approval", StepIndex: 1, State: "pending", Metadata: map[string]any{"suspension_id": susp.ID.String()}}
	require.NoError(t, f.store.CreateCheckpoint(leaseScope(), cp))
	r.State, r.StepCount = run.StatePaused, 1
	require.NoError(t, f.store.UpdateRun(leaseScope(), r))
	return r, susp, cp
}

// A ready run whose stored answer no check will ever accept is failed when
// it is continued, and its checkpoint closed, instead of staying ready and
// being announced forever.
func TestContinueRunFailsAReadyRunItCanNeverContinue(t *testing.T) {
	f := newLeaseFixture(t, newLeaseLLM())
	r, susp, cp := mustStoredPause(t, f)
	_, err := f.store.MarkReady(leaseScope(), r.ID, run.Readiness{Source: run.ReadyApproval, SuspensionID: susp.ID, CheckpointID: cp.ID,
		Results: []byte(`[{"tool_call_id":"call-y","execute":true}]`), At: time.Now().UTC()})
	require.NoError(t, err)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	b := reclaimer(t, f, bModel, hooks)
	_, err = b.ContinueRun(leaseScope(), r.ID)
	require.ErrorIs(t, err, cortex.ErrResultsMismatch)
	require.Zero(t, bModel.calls())
	got, err := f.store.GetRun(leaseScope(), r.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateFailed, got.State)
	require.Contains(t, got.Error, cortex.ErrResultsMismatch.Error())
	stored, err := f.store.GetCheckpoint(leaseScope(), cp.ID)
	require.NoError(t, err)
	require.NotEqual(t, "pending", stored.State, "the checkpoint of a failed run is still waiting on a decision")
	_, err = f.store.GetSuspension(leaseScope(), r.ID)
	require.ErrorIs(t, err, cortex.ErrNotSuspended)
	_, err = b.ContinueRun(leaseScope(), r.ID)
	require.ErrorIs(t, err, cortex.ErrRunAlreadyDone)
}

func TestSweeperClosesStaleCheckpoints(t *testing.T) {
	f := newLeaseFixture(t, newLeaseLLM())
	// Created first, so it sorts first and the walk has to get past it.
	live, liveSusp, liveCP := mustStoredPause(t, f)
	_ = liveSusp
	movedRun, _, _ := mustStoredPause(t, f)
	movedCP := &checkpoint.Checkpoint{ID: id.NewCheckpointID(), RunID: movedRun.ID, AgentID: movedRun.AgentID, Reason: "approval", State: "pending", Metadata: map[string]any{"suspension_id": id.NewSuspensionID().String()}}
	require.NoError(t, f.store.CreateCheckpoint(leaseScope(), movedCP))
	ended := storedRunningRun(t, f, run.Lease{Owner: "gone", ExpiresAt: time.Now().UTC().Add(time.Hour)})
	ended.State = run.StateCompleted
	require.NoError(t, f.store.UpdateRun(leaseScope(), ended))
	endedCP := &checkpoint.Checkpoint{ID: id.NewCheckpointID(), RunID: ended.ID, AgentID: ended.AgentID, Reason: "approval", State: "pending", Metadata: map[string]any{"suspension_id": id.NewSuspensionID().String()}}
	require.NoError(t, f.store.CreateCheckpoint(leaseScope(), endedCP))
	hostCP := &checkpoint.Checkpoint{ID: id.NewCheckpointID(), RunID: ended.ID, AgentID: ended.AgentID, Reason: "host review", State: "pending"}
	require.NoError(t, f.store.CreateCheckpoint(leaseScope(), hostCP))

	state := func(cp *checkpoint.Checkpoint) *checkpoint.Checkpoint {
		t.Helper()
		stored, err := f.store.GetCheckpoint(leaseScope(), cp.ID)
		require.NoError(t, err)
		return stored
	}
	// One checkpoint per sweep: enough sweeps to walk all five, and the
	// empty page that starts the walk over. The first engine's grace is an
	// hour, so only what is stale whatever the time is closed.
	patient := reclaimer(t, f, &reclaimLLM{}, newReclaimHooks(), engine.WithRunLease(time.Hour), engine.WithSuspensionSweepLimit(1), engine.WithWorkerID("patient"))
	for range 12 {
		engine.SweepOnce(patient, context.Background())
	}
	require.Equal(t, "resolved", state(movedCP).State, "a checkpoint for a pause its run has left is stale at once")
	require.Equal(t, "pending", state(endedCP).State, "a run written inside the grace may have a decider between its write and its record")

	b := reclaimer(t, f, &reclaimLLM{}, newReclaimHooks(), engine.WithRunLease(ownerlessGraceTTL), engine.WithSuspensionSweepLimit(1))
	pastGrace()
	for range 12 {
		engine.SweepOnce(b, context.Background())
	}
	closed := state(endedCP)
	require.Equal(t, "resolved", closed.State)
	require.NotNil(t, closed.Decision)
	require.False(t, closed.Decision.Approved)
	require.Equal(t, "cortex", closed.Decision.DecidedBy)
	require.Contains(t, closed.Decision.Reason, "superseded")
	require.Equal(t, "pending", state(liveCP).State, "the checkpoint of the pause its run waits on is live")
	require.Equal(t, "pending", state(hostCP).State, "a host's own checkpoint is the host's to close")
	require.Equal(t, run.StatePaused, func() run.State {
		r, err := f.store.GetRun(leaseScope(), live.ID)
		require.NoError(t, err)
		return r.State
	}())
}

// toolEventRecorder records the tool events of one run, in order.
type toolEventRecorder struct {
	mu     sync.Mutex
	events []string
}

func (*toolEventRecorder) Name() string { return "tool-events" }

func (r *toolEventRecorder) add(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *toolEventRecorder) OnToolCalled(_ context.Context, _ id.AgentRunID, name string, _ any) error {
	r.add("called:" + name)
	return nil
}

func (r *toolEventRecorder) OnToolCompleted(_ context.Context, _ id.AgentRunID, name, _ string, _ time.Duration) error {
	r.add("completed:" + name)
	return nil
}

func (r *toolEventRecorder) OnToolFailed(_ context.Context, _ id.AgentRunID, name string, _ error) error {
	r.add("failed:" + name)
	return nil
}

func (r *toolEventRecorder) OnToolDenied(_ context.Context, _ id.AgentRunID, name, _ string) error {
	r.add("denied:" + name)
	return nil
}

func (r *toolEventRecorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// denyWrite refuses every call to write and lets the rest through.
type denyWrite struct{}

func (denyWrite) Visible(_ context.Context, _ cortex.Subject, tools []llm.Tool) []llm.Tool {
	return tools
}

func (denyWrite) Authorize(_ context.Context, _ cortex.Subject, call llm.ToolCall) error {
	if call.Name == "write" {
		return errors.New("not today")
	}
	return nil
}

// A call that is never dispatched because its Started record could not be
// saved announces nothing: ToolCalled waits for the record. A call that
// gets there, or ends any other way, still gets its ToolCalled first.
func TestToolCalledWaitsForTheStartedRecord(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int32
		opts     []engine.Option
		want     []string
	}{
		{"recorded", 0, nil, []string{"called:write", "completed:write"}},
		{"not recorded", 2, nil, nil},
		{"denied", 0, []engine.Option{engine.WithToolAuthorizer(denyWrite{}), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[leasePrincipal]())}, []string{"called:write", "denied:write"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-1", Name: "write", Arguments: `{}`}))
			var ran atomic.Int32
			events := &toolEventRecorder{}
			opts := append([]engine.Option{engine.WithRunLease(300 * time.Millisecond), engine.WithExtension(events),
				engine.WithTool(llm.Tool{Name: "write", Description: "writes"}, func(context.Context, cortex.Invocation) (string, error) {
					ran.Add(1)
					return "written", nil
				})}, tc.opts...)
			f := newLeaseFixture(t, model, opts...)
			f.store.failStarted.Store(tc.failures)
			_, _ = f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
			require.Equal(t, tc.want, events.seen())
		})
	}
}
