package engine

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/suspension"
)

// leaseProbe is a tool that, while it runs, reads the run it was called
// for and that run's progress, so a test can see what the run held
// mid-execution.
type leaseProbe struct {
	st       *sqlitestore.Store
	run      atomic.Pointer[run.Run]
	progress atomic.Pointer[suspension.Progress]
}

func (p *leaseProbe) handler(ctx context.Context, inv cortex.Invocation) (string, error) {
	current, err := p.st.GetRun(ctx, inv.RunID)
	if err != nil {
		return "", err
	}
	p.run.Store(current)
	if saved, err := p.st.GetProgress(ctx, inv.RunID); err == nil {
		p.progress.Store(saved)
	}
	return "probed", nil
}

func probeTool() llm.Tool { return llm.Tool{Name: "probe", Description: "reads its own run"} }

// A run continued by ContinueRun, or resumed by Resume, runs under this
// worker's lease and saves progress, exactly like a new run, and ends
// with neither.
func TestContinuedAndResumedRunsHoldLeases(t *testing.T) {
	cases := map[string]llm.ToolCall{
		"continue after an approval":   {ID: "call-1", Name: gatedTool().Name, Arguments: `{}`},
		"resume with external results": {ID: "call-1", Name: externalTool().Name, Arguments: `{}`},
	}
	for name, first := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := cortex.WithScope(context.Background(), approvalScope())
			st := newApprovalStore(ctx, t)
			probe := &leaseProbe{st: st}
			f := buildContinueFixture(ctx, t, st, []llm.ToolCall{first}, WithTool(probeTool(), probe.handler), WithRunLease(60*time.Second))
			f.model.then = [][]llm.ToolCall{{{ID: "call-2", Name: probeTool().Name, Arguments: `{}`}}}
			f.overrides = &RunOverrides{MaxSteps: 5}
			paused, cp := f.pause(t)

			var done *run.Run
			var err error
			if cp != nil {
				require.NoError(t, f.e.DecideCheckpoint(as(ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
				done, err = f.e.ContinueRun(ctx, paused.ID)
			} else {
				done, err = f.e.Resume(ctx, paused.ID, ResumeInput{ToolResults: []ToolResult{{ToolCallID: "call-1", Content: "answer"}}})
			}
			require.NoError(t, err)
			require.Equal(t, run.StateCompleted, done.State)

			during := probe.run.Load()
			require.NotNil(t, during, "the probe did not run")
			require.NotNil(t, during.Lease, "the continued run held no lease")
			require.Equal(t, f.e.WorkerID(), during.Lease.Owner)
			saved := probe.progress.Load()
			require.NotNil(t, saved, "the continued run saved no progress")
			require.Equal(t, []string{"call-2"}, pendingIDs(saved.Started))

			after := f.runState(t, paused.ID)
			require.Nil(t, after.Lease)
			_, err = st.GetProgress(ctx, paused.ID)
			require.ErrorIs(t, err, cortex.ErrProgressNotFound)
		})
	}
}

func pendingIDs(calls []suspension.PendingCall) []string {
	out := make([]string, 0, len(calls))
	for _, c := range calls {
		out = append(out, c.ID)
	}
	return out
}

// orderedResolveStore makes the first checkpoint Resolve wait until a
// second one has arrived, and the second wait until the first has
// written, so a test can pin which of two deciders records first.
type orderedResolveStore struct {
	*sqlitestore.Store
	calls    atomic.Int32
	inFirst  atomic.Pointer[func()]
	arrived  chan struct{}
	recorded chan struct{}
}

func (s *orderedResolveStore) Resolve(ctx context.Context, cpID id.CheckpointID, d checkpoint.Decision) error {
	switch s.calls.Add(1) {
	case 1:
		if hook := s.inFirst.Load(); hook != nil {
			(*hook)()
		}
		<-s.arrived
		err := s.Store.Resolve(ctx, cpID, d)
		close(s.recorded)
		return err
	case 2:
		close(s.arrived)
		<-s.recorded
	}
	return s.Store.Resolve(ctx, cpID, d)
}

// The same decider approving twice at once: the call whose MarkReady
// moved the run records first and announces it; the repeat finds its own
// decision recorded, succeeds, and announces nothing.
func TestSameDeciderTwiceAnnouncesReadyOnce(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := &orderedResolveStore{Store: newApprovalStore(ctx, t), arrived: make(chan struct{}), recorded: make(chan struct{})}
	f := buildContinueFixture(ctx, t, st, []llm.ToolCall{{ID: "call-1", Name: gatedTool().Name, Arguments: `{}`}})
	paused, cp := f.pause(t)
	decision := checkpoint.Decision{Approved: true, DecidedBy: "bob"}

	repeat := make(chan error, 1)
	second := func() {
		go func() { repeat <- f.e.DecideCheckpoint(as(ctx, bob), cp.ID, decision) }()
	}
	st.inFirst.Store(&second)

	require.NoError(t, f.e.DecideCheckpoint(as(ctx, bob), cp.ID, decision))
	require.NoError(t, <-repeat)
	_, notices, _ := f.ready.snapshot()
	require.Len(t, notices, 1, "RunReady fired for each of two identical decisions")
	require.Len(t, f.cps.resolvedEvents(), 1)
	require.Equal(t, run.StateReady, f.runState(t, paused.ID).State)

	done, err := f.e.ContinueRun(ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)
	require.EqualValues(t, 1, f.ran.Load())
}

// staleSuspensionStore serves one stale suspension read when armed, the
// way a decider that read the run a moment before it paused again sees it.
type staleSuspensionStore struct {
	*flakyResolveStore
	stale atomic.Pointer[suspension.Suspension]
}

func (s *staleSuspensionStore) GetSuspension(ctx context.Context, runID id.AgentRunID) (*suspension.Suspension, error) {
	if stale := s.stale.Swap(nil); stale != nil {
		return stale, nil
	}
	return s.flakyResolveStore.GetSuspension(ctx, runID)
}

// A default rejection claims only the pause its checkpoint was opened
// for. A stale rejection whose run has since paused again on another
// suspension is refused with ErrInvalidState and fails nothing.
func TestStaleRejectionCannotFailTheNextPause(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := &staleSuspensionStore{flakyResolveStore: &flakyResolveStore{Store: newApprovalStore(ctx, t)}}
	f := buildContinueFixture(ctx, t, st, []llm.ToolCall{{ID: "call-1", Name: gatedTool().Name, Arguments: `{}`}})
	f.model.then = [][]llm.ToolCall{{{ID: "call-2", Name: gatedTool().Name, Arguments: `{"again":true}`}}}
	f.overrides = &RunOverrides{MaxSteps: 5}
	paused, cp1 := f.pause(t)
	s1, err := st.Store.GetSuspension(ctx, paused.ID)
	require.NoError(t, err)

	// Both writes of cp1's approval are lost, so cp1 stays pending while
	// the run carries the approval out and pauses again on s2.
	st.failures.Store(2)
	require.Error(t, f.e.DecideCheckpoint(as(ctx, bob), cp1.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
	again, err := f.e.ContinueRun(ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, again.State)
	s2, err := st.Store.GetSuspension(ctx, paused.ID)
	require.NoError(t, err)
	require.NotEqual(t, s1.ID, s2.ID)

	// The rejecter read the run while it still waited on s1.
	st.stale.Store(s1)
	err = f.e.DecideCheckpoint(as(ctx, carol), cp1.ID, checkpoint.Decision{DecidedBy: "carol", Reason: "stale"})
	require.ErrorIs(t, err, cortex.ErrInvalidState)
	current := f.runState(t, paused.ID)
	require.Equal(t, run.StatePaused, current.State, "a stale rejection failed the run's next pause")
	still, err := st.Store.GetSuspension(ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, s2.ID, still.ID)
	require.Zero(t, f.ended.of(paused.ID))
	require.EqualValues(t, 1, f.ran.Load())
}
