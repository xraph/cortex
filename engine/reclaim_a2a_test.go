package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/suspension"
)

// stallingAdmitStore is a SQLite store whose first leased agent-reply
// start commits and then blocks, as a worker that dies right after it was
// granted the reply's resume would.
type stallingAdmitStore struct {
	*sqlitestore.Store
	stalled  atomic.Bool
	admitted chan struct{}
	release  chan struct{}
	once     sync.Once
	// stallAdmission stalls after the leased admission instead of after
	// the leased start.
	stallAdmission bool
	// failEnds fails that many writes of a cancelled run.
	failEnds atomic.Int32
}

func (s *stallingAdmitStore) stall() {
	if s.stalled.CompareAndSwap(false, true) {
		s.admitted <- struct{}{}
		<-s.release
	}
}

func (s *stallingAdmitStore) AdmitResumeLeased(ctx context.Context, req suspension.ResumeAdmissionRequest, lease run.Lease, first *suspension.Progress) (suspension.ResumeAdmission, error) {
	admitted, err := s.Store.AdmitResumeLeased(ctx, req, lease, first)
	if err == nil && s.stallAdmission {
		s.stall()
	}
	return admitted, err
}

// AdmitResume stalls the same way, so a test can tell an admission that
// took no lease from one that did.
func (s *stallingAdmitStore) AdmitResume(ctx context.Context, req suspension.ResumeAdmissionRequest) (suspension.ResumeAdmission, error) {
	admitted, err := s.Store.AdmitResume(ctx, req)
	if err == nil && s.stallAdmission {
		s.stall()
	}
	return admitted, err
}

func (s *stallingAdmitStore) UpdateRun(ctx context.Context, r *run.Run) error {
	if r.State == run.StateCancelled && s.failEnds.Add(-1) >= 0 {
		return errors.New("store unavailable")
	}
	return s.Store.UpdateRun(ctx, r)
}

func (s *stallingAdmitStore) StartResumeLeased(ctx context.Context, runID id.AgentRunID, key id.SuspensionID, digest string, lease run.Lease, first *suspension.Progress) (suspension.ResumeStart, error) {
	started, err := s.Store.StartResumeLeased(ctx, runID, key, digest, lease, first)
	if err == nil && !s.stallAdmission {
		s.stall()
	}
	return started, err
}

func (s *stallingAdmitStore) letGo() { s.once.Do(func() { close(s.release) }) }

// A run waiting on a peer's answer gets it, and the worker carrying the
// answer dies right after its claim. The claim wrote the answer into the
// run's first snapshot, so the replica that reclaims the run hands it to
// the model and does not ask the peer a second time.
func TestReclaimAfterARecordedReplyDoesNotAskAgain(t *testing.T) {
	ctx := a2aScope()
	st := &stallingAdmitStore{Store: newApprovalStore(ctx, t), admitted: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(st.letGo)
	model := newDrainingLLM(false)
	t.Cleanup(model.letGo)
	first := newDrainingEngine(ctx, t, st, model, true, WithRunLease(200*time.Millisecond))

	paused, err := first.RunAgent(ctx, "planner", "find out how the worker is doing", nil)
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("RunAgent = %v, %v; want a paused planner", paused, err)
	}
	drained := make(chan error, 1)
	go func() {
		_, drainErr := first.A2A().Drain(ctx)
		drained <- drainErr
	}()
	waitSignal(t, st.admitted, "the claim of the planner's reply")
	waitLeaseLapsed(t, st, paused)

	replica := newDrainingEngine(ctx, t, st, model, false, WithWorkerID("replica"))
	replica.sweepLapsed(context.Background(), time.Now().UTC())
	continued, err := replica.ContinueRun(ctx, paused.ID)
	if err != nil || continued.State != run.StateCompleted {
		t.Fatalf("ContinueRun = %+v, %v; want the planner completed", continued, err)
	}
	if !strings.Contains(continued.Output, "all clear") {
		t.Fatalf("planner output = %q, want the worker's recorded answer", continued.Output)
	}
	if _, drainErr := replica.A2A().Drain(ctx); drainErr != nil {
		t.Fatalf("replica Drain: %v", drainErr)
	}
	if got := model.workerCalls.Load(); got != 1 {
		t.Fatalf("the worker was asked %d times, want once", got)
	}

	st.letGo()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled worker's Drain did not return")
	}
	if got := model.workerCalls.Load(); got != 1 {
		t.Fatalf("after the stalled worker woke, the worker was asked %d times, want once", got)
	}
}

func waitLeaseLapsed(t *testing.T, st *stallingAdmitStore, r *run.Run) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := st.GetRun(a2aScope(), r.ID)
		if err == nil && current.Lease != nil && time.Now().After(current.Lease.ExpiresAt) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the claimed run's lease never lapsed")
}

// A directive's run interrupted by Stop's deadline sends no Failure. It is
// reclaimed and continued on another engine, and that engine sends the
// answer: the asker completes on what the recipient said.
func TestHandedOffDirectiveAnswersFromTheReplicaThatFinishesIt(t *testing.T) {
	ctx := a2aScope()
	st := newApprovalStore(ctx, t)
	model := newDrainingLLM(true)
	t.Cleanup(model.letGo)
	stopping := newDrainingEngine(ctx, t, st, model, true)

	paused, err := stopping.RunAgent(ctx, "planner", "find out how the worker is doing", nil)
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("RunAgent = %v, %v; want a paused planner", paused, err)
	}
	drained := make(chan error, 1)
	go func() {
		_, drainErr := stopping.A2A().Drain(ctx)
		drained <- drainErr
	}()
	waitSignal(t, model.workerIn, "the worker's run")

	// Stop runs out of time while the worker's run is in its model call:
	// the run is handed off, reclaimable, and nobody has answered.
	deadline, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if stopErr := stopping.Stop(deadline); stopErr == nil {
		t.Fatal("Stop returned nil while the worker's run was still executing")
	}
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("Drain did not return after the hand-off")
	}
	t.Cleanup(func() { _ = stopping.Stop(context.Background()) })
	workers, err := st.ListRuns(ctx, &run.ListFilter{State: run.StateReclaimable})
	if err != nil || len(workers) != 1 {
		t.Fatalf("reclaimable runs = %d, %v; want the worker's run", len(workers), err)
	}
	if planner, readErr := st.GetRun(ctx, paused.ID); readErr != nil || planner.State != run.StatePaused {
		t.Fatalf("planner = %+v, %v; want it still waiting on the worker", planner, readErr)
	}

	model.letGo()
	replica := newDrainingEngine(ctx, t, st, model, false, WithWorkerID("replica"))
	finished, err := replica.ContinueRun(ctx, workers[0].ID)
	if err != nil || finished.State != run.StateCompleted {
		t.Fatalf("ContinueRun of the worker = %+v, %v; want completed", finished, err)
	}
	if _, drainErr := replica.A2A().Drain(ctx); drainErr != nil {
		t.Fatalf("replica Drain: %v", drainErr)
	}
	assertNoStoppingFailure(ctx, t, st, paused.ID)
}

// The worker that records a peer's reply dies after its admission and
// before it starts the resume. The admission took its lease and wrote the
// reply into the run's snapshot, so on an engine with no reply recovery
// configured the lease lapses, the sweep reclaims the run, and it
// completes on the reply without asking the peer again.
func TestAdmitterThatDiesBeforeItsStartIsReclaimed(t *testing.T) {
	ctx := a2aScope()
	st := &stallingAdmitStore{Store: newApprovalStore(ctx, t), admitted: make(chan struct{}, 1), release: make(chan struct{}), stallAdmission: true}
	t.Cleanup(st.letGo)
	model := newDrainingLLM(false)
	t.Cleanup(model.letGo)
	first := newDrainingEngine(ctx, t, st, model, true, WithRunLease(200*time.Millisecond))

	paused, err := first.RunAgent(ctx, "planner", "find out how the worker is doing", nil)
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("RunAgent = %v, %v; want a paused planner", paused, err)
	}
	drained := make(chan error, 1)
	go func() {
		_, drainErr := first.A2A().Drain(ctx)
		drained <- drainErr
	}()
	waitSignal(t, st.admitted, "the admission of the planner's reply")
	waitLeaseLapsed(t, st, paused)

	replica := newDrainingEngine(ctx, t, st, model, false, WithWorkerID("replica"))
	replica.sweepLapsed(context.Background(), time.Now().UTC())
	continued, err := replica.ContinueRun(ctx, paused.ID)
	if err != nil || continued.State != run.StateCompleted || !strings.Contains(continued.Output, "all clear") {
		t.Fatalf("ContinueRun = %+v, %v; want the planner completed on the worker's answer", continued, err)
	}
	if _, drainErr := replica.A2A().Drain(ctx); drainErr != nil {
		t.Fatalf("replica Drain: %v", drainErr)
	}
	st.letGo()
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatal("the stalled admitter's Drain did not return")
	}
	if got := model.workerCalls.Load(); got != 1 {
		t.Fatalf("the worker was asked %d times, want once", got)
	}
}

// A cancel claims a run waiting on a peer's answer and its write is lost.
// The reclaim finishes the cancel: the run ends cancelled and the peer is
// not asked a second time.
func TestLostCancelOfAPeerQuestionDoesNotAskAgain(t *testing.T) {
	ctx := a2aScope()
	st := &stallingAdmitStore{Store: newApprovalStore(ctx, t), admitted: make(chan struct{}, 1), release: make(chan struct{})}
	t.Cleanup(st.letGo)
	model := newDrainingLLM(false)
	t.Cleanup(model.letGo)
	first := newDrainingEngine(ctx, t, st, model, true, WithRunLease(200*time.Millisecond))

	paused, err := first.RunAgent(ctx, "planner", "find out how the worker is doing", nil)
	if err != nil || paused.State != run.StatePaused {
		t.Fatalf("RunAgent = %v, %v; want a paused planner", paused, err)
	}
	st.failEnds.Store(1)
	if cancelErr := first.CancelRun(ctx, paused.ID); cancelErr == nil {
		t.Fatal("CancelRun succeeded though its write was dropped")
	}
	waitLeaseLapsed(t, st, paused)

	replica := newDrainingEngine(ctx, t, st, model, false, WithWorkerID("replica"))
	replica.sweepLapsed(context.Background(), time.Now().UTC())
	ended, err := replica.ContinueRun(ctx, paused.ID)
	if !errors.Is(err, cortex.ErrRunAlreadyDone) || ended == nil || ended.State != run.StateCancelled {
		t.Fatalf("ContinueRun = %+v, %v; want the cancel finished", ended, err)
	}
	asks, err := st.ListMessages(ctx, &a2a.MessageListFilter{Limit: 20})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	questions := 0
	for _, m := range asks {
		if m.Sender.Agent == "planner" {
			questions++
		}
	}
	if questions != 1 {
		t.Fatalf("the planner sent %d questions, want the original one only", questions)
	}
}

// The snapshot a claim from a pause writes records every call that left
// the engine and is not answered as started, so a reclaim never sends it
// again, and records nothing as started for an approval nobody sent.
func TestClaimSnapshotRecordsSentCallsAsStarted(t *testing.T) {
	asked := &suspension.Suspension{ID: id.NewSuspensionID(), RunID: id.NewAgentRunID(), Reason: suspension.ReasonAgentReply,
		Pending: []suspension.PendingCall{{ID: "ask-1", Name: toolAgentAsk}, {ID: "ask-2", Name: toolAgentAsk}}}
	snap := claimSnapshot(asked, []suspension.ProgressResult{{CallID: "ask-1", Content: "answer", Outcome: "completed"}})
	if len(snap.Started) != 1 || snap.Started[0].ID != "ask-2" || len(snap.Done) != 1 {
		t.Fatalf("claim snapshot of a peer question = started %+v done %+v; want ask-2 started, ask-1 done", snap.Started, snap.Done)
	}
	external := &suspension.Suspension{ID: id.NewSuspensionID(), RunID: id.NewAgentRunID(), Reason: suspension.ReasonExternalTool,
		Pending: []suspension.PendingCall{{ID: "call-x", Name: "lookup"}}}
	if snap = claimSnapshot(external, nil); len(snap.Started) != 1 || snap.Started[0].ID != "call-x" {
		t.Fatalf("claim snapshot of an external call = started %+v; want it started", snap.Started)
	}
	approval := &suspension.Suspension{ID: id.NewSuspensionID(), RunID: id.NewAgentRunID(), Reason: suspension.ReasonApproval,
		Pending: []suspension.PendingCall{{ID: "call-c", Name: "c"}}}
	if snap = claimSnapshot(approval, nil); len(snap.Started) != 0 {
		t.Fatalf("claim snapshot of an approval = started %+v; want nothing started", snap.Started)
	}
}
