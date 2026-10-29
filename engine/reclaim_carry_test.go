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

// A running run that holds no lease is what a v1.9 replica's run looks
// like in a mixed fleet: that replica takes no leases and never rewrites
// the run row while it works. No sweep ever touches one, however long it
// has been idle, and ContinueRun refuses it as still running.
func TestSweeperLeavesUnleasedRunningRunsAlone(t *testing.T) {
	f := newLeaseFixture(t, newLeaseLLM())
	ag, err := f.store.GetByName(leaseScope(), "assistant")
	require.NoError(t, err)
	legacy := &run.Run{Entity: cortex.NewEntity(), ID: id.NewAgentRunID(), AgentID: ag.ID, State: run.StateRunning, Input: "work"}
	require.NoError(t, f.store.CreateRun(leaseScope(), legacy))

	b := reclaimer(t, f, &reclaimLLM{}, newReclaimHooks(), engine.WithRunLease(ownerlessGraceTTL))
	pastGrace()
	for range 3 {
		engine.SweepOnce(b, context.Background())
	}
	got, err := f.store.GetRun(leaseScope(), legacy.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, got.State)
	require.Zero(t, got.Reclaims)
	_, err = b.ContinueRun(leaseScope(), legacy.ID)
	require.ErrorIs(t, err, cortex.ErrRunLeased)
	got, err = f.store.GetRun(leaseScope(), legacy.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, got.State)
}

// stallAfterClaim makes the fixture's next leased claim commit and then
// block, as a worker that dies right after its claim would, until the test
// lets it go.
func stallAfterClaim(t *testing.T, f *leaseFixture) (claimed <-chan struct{}, letGo func()) {
	t.Helper()
	in, out := make(chan struct{}, 1), make(chan struct{})
	var once sync.Once
	var stalled atomic.Bool
	f.store.afterClaim = func() {
		// Only the first claim stalls: the replica that reclaims the run
		// claims through the same store.
		if !stalled.CompareAndSwap(false, true) {
			return
		}
		in <- struct{}{}
		<-out
	}
	release := func() { once.Do(func() { close(out) }) }
	t.Cleanup(release)
	return in, release
}

// A worker that claims a pause and dies before it does anything else
// leaves a lease that lapses, and the snapshot the claim wrote. The sweep
// reclaims the run and another replica continues it with the answer the
// resume carried: the model sees it, and nothing asks for it again.
func TestResumeClaimThenCrashRecoversThroughTheSweep(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-x", Name: "lookup", Arguments: `{"q":1}`}))
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL), engine.WithExternalTool(llm.Tool{Name: "lookup"}))
	paused, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, paused.State)

	claimed, letGo := stallAfterClaim(t, f)
	done := make(chan runOutcome, 1)
	f.runs.Add(1)
	go func() {
		defer f.runs.Done()
		r, resumeErr := f.eng.Resume(leaseScope(), paused.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "call-x", Content: "forty-two"}}})
		done <- runOutcome{run: r, err: resumeErr}
	}()
	waitSignal(t, claimed, "the resume's claim")
	waitLapsed(t, f, paused.ID)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	b := reclaimer(t, f, bModel, hooks, engine.WithExternalTool(llm.Tool{Name: "lookup"}))
	engine.SweepOnce(b, context.Background())
	require.Len(t, hooks.snapshot().reclaimable, 1)
	continued, err := b.ContinueRun(leaseScope(), paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, continued.State)
	require.Equal(t, "forty-two", lastToolMessage(t, bModel.firstRequest(t)).Content)
	require.Empty(t, hooks.snapshot().suspended, "the answered call was asked for again")

	letGo()
	require.Error(t, waitOutcome(t, done).err, "the stalled worker went on to finish a run it no longer holds")
}

// The same for a run continued from a job: ProvideResults made it ready,
// the job's claim commits and the job dies. The answer survives.
func TestProvidedResultsSurviveAJobThatDiesAfterItsClaim(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-x", Name: "lookup", Arguments: `{"q":1}`}))
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL), engine.WithExternalTool(llm.Tool{Name: "lookup"}))
	paused, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
	require.NoError(t, err)
	require.NoError(t, f.eng.ProvideResults(leaseScope(), paused.ID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "call-x", Content: "forty-two"}}}))

	claimed, letGo := stallAfterClaim(t, f)
	done := make(chan runOutcome, 1)
	f.runs.Add(1)
	go func() {
		defer f.runs.Done()
		r, continueErr := f.eng.ContinueRun(leaseScope(), paused.ID)
		done <- runOutcome{run: r, err: continueErr}
	}()
	waitSignal(t, claimed, "the job's claim")
	waitLapsed(t, f, paused.ID)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	b := reclaimer(t, f, bModel, hooks, engine.WithExternalTool(llm.Tool{Name: "lookup"}))
	engine.SweepOnce(b, context.Background())
	continued, err := b.ContinueRun(leaseScope(), paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, continued.State)
	require.Equal(t, "forty-two", lastToolMessage(t, bModel.firstRequest(t)).Content)
	require.Empty(t, hooks.snapshot().suspended, "the answered call was asked for again")

	letGo()
	require.Error(t, waitOutcome(t, done).err)
}

// waitLapsed waits until the run's lease has expired in the store.
func waitLapsed(t *testing.T, f *leaseFixture, runID id.AgentRunID) {
	t.Helper()
	require.Eventually(t, func() bool {
		r, err := f.store.GetRun(leaseScope(), runID)
		return err == nil && r.Lease != nil && time.Now().After(r.Lease.ExpiresAt)
	}, 5*time.Second, 5*time.Millisecond, "the claimed run's lease never lapsed")
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A pause whose calls already left the engine (here a question to an
// external tool's host), reclaimed with no snapshot to say what came of
// them, is not sent again: the call is treated as started with an unknown
// outcome, and the run stops as uncertain.
func TestReclaimWithoutProgressDoesNotResendAPause(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-x", Name: "lookup", Arguments: `{"q":1}`}))
	f := newLeaseFixture(t, model, engine.WithExternalTool(llm.Tool{Name: "lookup"}))
	paused, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
	require.NoError(t, err)
	// A claim with no snapshot behind it, under a lease that has lapsed.
	_, err = f.store.ClaimSuspension(leaseScope(), paused.ID)
	require.NoError(t, err)
	require.NoError(t, f.store.HoldLease(leaseScope(), paused.ID, run.Lease{Owner: "dead", ExpiresAt: time.Now().UTC().Add(-time.Second)}))

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	b := reclaimer(t, f, bModel, hooks, engine.WithExternalTool(llm.Tool{Name: "lookup"}))
	engine.SweepOnce(b, context.Background())
	_, err = b.ContinueRun(leaseScope(), paused.ID)
	require.ErrorIs(t, err, cortex.ErrEffectUncertain)
	require.Contains(t, err.Error(), `"lookup"`)
	require.Empty(t, hooks.snapshot().suspended, "the call was handed to the host a second time")
	require.Zero(t, bModel.calls())
	got, err := f.store.GetRun(leaseScope(), paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateIncomplete, got.State)
	require.Equal(t, "lookup", got.Metadata["uncertain_tool"])
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

// A cancel claims a run paused on an external call and its terminal write
// is lost. The claim took the canceller's lease and recorded the cancel,
// so once the lease lapses the run is reclaimed and the cancel finished:
// the host is not handed the call again and the model is not called.
func TestLostCancelIsFinishedOnReclaim(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-x", Name: "lookup", Arguments: `{"q":1}`}))
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL), engine.WithExternalTool(llm.Tool{Name: "lookup"}))
	paused, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, paused.State)

	f.store.failEnds.Store(1)
	require.Error(t, f.eng.CancelRun(leaseScope(), paused.ID), "the cancel's write was dropped")
	waitLapsed(t, f, paused.ID)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	b := reclaimer(t, f, bModel, hooks, engine.WithExternalTool(llm.Tool{Name: "lookup"}))
	engine.SweepOnce(b, context.Background())
	ended, err := b.ContinueRun(leaseScope(), paused.ID)
	require.ErrorIs(t, err, cortex.ErrRunAlreadyDone)
	require.Equal(t, run.StateCancelled, ended.State)
	require.Empty(t, hooks.snapshot().suspended, "the external call was handed to the host again")
	require.Zero(t, bModel.calls())
	_, err = f.store.GetSuspension(leaseScope(), paused.ID)
	require.ErrorIs(t, err, cortex.ErrNotSuspended)
	got, err := f.store.GetRun(leaseScope(), paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCancelled, got.State)
}

// The same for an expiry: the sweep claims a run whose external call
// nobody answered in time and its failure write is lost. The reclaim fails
// the run with the expiry, and does not hand the call out again.
func TestLostExpiryIsFinishedOnReclaim(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-x", Name: "lookup", Arguments: `{"q":1}`}))
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL), engine.WithExternalTool(llm.Tool{Name: "lookup"}, engine.ExternalTTL(20*time.Millisecond)))
	paused, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, paused.State)
	time.Sleep(40 * time.Millisecond)

	f.store.failEnds.Store(1)
	engine.SweepOnce(f.eng, context.Background())
	claimed, err := f.store.GetRun(leaseScope(), paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateRunning, claimed.State, "the expiry's failure write was dropped")
	waitLapsed(t, f, paused.ID)

	hooks := newReclaimHooks()
	bModel := &reclaimLLM{}
	b := reclaimer(t, f, bModel, hooks, engine.WithExternalTool(llm.Tool{Name: "lookup"}))
	engine.SweepOnce(b, context.Background())
	ended, err := b.ContinueRun(leaseScope(), paused.ID)
	require.ErrorIs(t, err, cortex.ErrSuspensionExpired)
	require.Equal(t, run.StateFailed, ended.State)
	require.Empty(t, hooks.snapshot().suspended, "the external call was handed to the host again")
	require.Zero(t, bModel.calls())
	got, err := f.store.GetRun(leaseScope(), paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateFailed, got.State)
	require.Contains(t, got.Error, "nothing answered")
}

// A cancel of a run paused for an approval closes its checkpoint with the
// cancel's reason when the reclaim finishes it.
func TestLostCancelClosesTheCheckpointOnReclaim(t *testing.T) {
	model := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-c", Name: "c", Arguments: `{}`}))
	f := newLeaseFixture(t, model, engine.WithRunLease(reclaimTTL),
		engine.WithTool(llm.Tool{Name: "c"}, (&countingTool{}).handler),
		engine.WithToolAuthorizer(approveC{}), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[leasePrincipal]()))
	paused, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, paused.State)
	cps, err := f.store.ListPending(leaseScope(), &checkpoint.ListFilter{RunID: paused.ID.String()})
	require.NoError(t, err)
	require.Len(t, cps, 1)

	f.store.failEnds.Store(1)
	require.Error(t, f.eng.CancelRun(leaseScope(), paused.ID))
	waitLapsed(t, f, paused.ID)
	b := reclaimer(t, f, &reclaimLLM{}, newReclaimHooks(),
		engine.WithTool(llm.Tool{Name: "c"}, (&countingTool{}).handler),
		engine.WithToolAuthorizer(approveC{}), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[leasePrincipal]()))
	engine.SweepOnce(b, context.Background())
	_, err = b.ContinueRun(leaseScope(), paused.ID)
	require.ErrorIs(t, err, cortex.ErrRunAlreadyDone)
	closed, err := f.store.GetCheckpoint(leaseScope(), cps[0].ID)
	require.NoError(t, err)
	require.Equal(t, "resolved", closed.State)
	require.Contains(t, closed.Decision.Reason, "cancelled")
}
