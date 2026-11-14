package engine_test

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
)

// deferOnceLLM is a leaseLLM whose call number at (0-based, counting
// every call) is deferred. The deferred call reaches no response, so the
// call after it gets the response the deferred one would have had.
type deferOnceLLM struct {
	*leaseLLM
	at   int32
	seen atomic.Int32
}

func (m *deferOnceLLM) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	if m.seen.Add(1)-1 == m.at {
		return nil, cortex.DeferredModelCallError{Kind: "credits", Reason: "The workspace is out of AI credits."}
	}
	return m.leaseLLM.Complete(ctx, req)
}

// A deferral pauses a leased run like any other pause: the lease and the
// progress go with it. Continued, the run is claimed under a lease, and
// when the worker carrying it out stalls in the retried model call, the
// sweep reclaims it and another replica finishes it from the pause,
// without running the tool again.
func TestModelCallDeferral_reclaimableUnderLeases(t *testing.T) {
	lookup := llm.Tool{Name: "lookup", Description: "test-only tool that reads something"}
	var lookups atomic.Int32
	handler := func(context.Context, cortex.Invocation) (string, error) {
		lookups.Add(1)
		return "found", nil
	}

	inner := newLeaseLLM(toolCalls(llm.ToolCall{ID: "call-1", Name: lookup.Name, Arguments: `{}`}))
	// The retried call is the leaseLLM's second.
	inner.hold[1] = true
	model := &deferOnceLLM{leaseLLM: inner, at: 1}
	f := newLeaseFixture(t, inner, engine.WithRunLease(reclaimTTL), engine.WithLLM(model), engine.WithTool(lookup, handler))

	paused, err := f.eng.RunAgent(leaseScope(), "assistant", "work", nil)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, paused.State)
	require.Nil(t, f.onlyRun(t).Lease, "a paused run kept its lease")
	_, err = f.store.GetProgress(leaseScope(), paused.ID)
	require.ErrorIs(t, err, cortex.ErrProgressNotFound)

	cps, err := f.eng.ListPendingCheckpoints(leaseScope(), &checkpoint.ListFilter{RunID: paused.ID.String()})
	require.NoError(t, err)
	require.Len(t, cps, 1)
	require.Equal(t, checkpoint.KindDeferredModelCall, cps[0].Metadata[checkpoint.MetadataKind])
	require.NoError(t, f.eng.DecideCheckpoint(leaseScope(), cps[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))

	done := make(chan runOutcome, 1)
	f.runs.Add(1)
	go func() {
		defer f.runs.Done()
		r, continueErr := f.eng.ContinueRun(leaseScope(), paused.ID)
		done <- runOutcome{run: r, err: continueErr}
	}()
	inner.waitEntered(t)
	claimed := f.onlyRun(t)
	require.NotNil(t, claimed.Lease, "the continued run held no lease")
	require.Equal(t, f.eng.WorkerID(), claimed.Lease.Owner)

	stall(t, f, paused.ID)
	bModel := &reclaimLLM{}
	b := reclaimer(t, f, bModel, newReclaimHooks(), engine.WithTool(lookup, handler))
	engine.SweepOnce(b, context.Background())
	continued, err := b.ContinueRun(leaseScope(), paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, continued.State)
	require.Equal(t, 1, bModel.calls(), "the reclaiming replica did not make exactly the retried call")
	require.Equal(t, "found", lastToolMessage(t, bModel.firstRequest(t)).Content, "the tool result before the deferral was lost")
	require.EqualValues(t, 1, lookups.Load(), "the tool ran again")

	inner.letGo()
	if out := waitOutcome(t, done); out.err == nil {
		require.Equal(t, continued.Output, out.run.Output)
	} else {
		require.ErrorIs(t, out.err, cortex.ErrRunLeaseLost)
	}
	final := f.onlyRun(t)
	require.Equal(t, run.StateCompleted, final.State)
	require.Equal(t, continued.Output, final.Output)
}
