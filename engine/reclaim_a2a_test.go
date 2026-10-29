package engine

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
}

func (s *stallingAdmitStore) StartResumeLeased(ctx context.Context, runID id.AgentRunID, key id.SuspensionID, digest string, lease run.Lease, first *suspension.Progress) (suspension.ResumeStart, error) {
	started, err := s.Store.StartResumeLeased(ctx, runID, key, digest, lease, first)
	if err == nil && s.stalled.CompareAndSwap(false, true) {
		s.admitted <- struct{}{}
		<-s.release
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
