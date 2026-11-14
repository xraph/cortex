package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

// deferralLLM serves its scripted responses in order and answers the
// calls named in deferAt (1-based) with that error instead. A deferred
// call consumes no response, so the call after it gets the response the
// deferred one would have had. It keeps a copy of every request's
// messages.
type deferralLLM struct {
	mu        sync.Mutex
	responses []*llm.Response
	deferAt   map[int]error
	// onNext makes a streamed deferral surface from the stream's first
	// Next rather than from opening the stream.
	onNext   bool
	served   int
	requests [][]llm.Message
}

func (m *deferralLLM) next(req *llm.Request) (*llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, slices.Clone(req.Messages))
	if err, ok := m.deferAt[len(m.requests)]; ok {
		return nil, err
	}
	resp := &llm.Response{Content: "done", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 7}}
	if m.served < len(m.responses) {
		resp = m.responses[m.served]
	}
	m.served++
	return resp, nil
}

func (m *deferralLLM) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	return m.next(req)
}

func (m *deferralLLM) CompleteStream(_ context.Context, req *llm.Request) (llm.Stream, error) {
	resp, err := m.next(req)
	if err != nil && m.onNext {
		return &refusedStream{err: err}, nil
	}
	if err != nil {
		return nil, err
	}
	return &outcomeStream{response: resp}, nil
}

func (m *deferralLLM) calls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}

func (m *deferralLLM) request(t *testing.T, i int) []llm.Message {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	require.Greater(t, len(m.requests), i, "the model got no request %d", i)
	return m.requests[i]
}

// refusedStream fails its first Next with err, before any chunk.
type refusedStream struct{ err error }

func (s *refusedStream) Next(context.Context) (*llm.Chunk, error) { return nil, s.err }
func (*refusedStream) Close() error                               { return nil }
func (*refusedStream) Usage() *llm.Usage                          { return nil }

func lookupTool() llm.Tool {
	return llm.Tool{Name: "lookup", Description: "test-only tool that reads something"}
}

func creditsDeferral() error {
	return cortex.DeferredModelCall{Kind: "credits", Reason: "The workspace is out of AI credits."}
}

// deferralFixture is a continueFixture whose model calls the lookup tool
// once and then answers, deferring the calls named in deferAt.
type deferralFixture struct {
	*continueFixture
	model   *deferralLLM
	lookups atomic.Int32
}

func newDeferralFixture(t *testing.T, deferAt map[int]error, opts ...Option) *deferralFixture {
	t.Helper()
	ctx := cortex.WithScope(context.Background(), approvalScope())
	return deferralFixtureOn(ctx, t, newApprovalStore(ctx, t), deferAt, opts...)
}

func deferralFixtureOn(ctx context.Context, t *testing.T, st store.Store, deferAt map[int]error, opts ...Option) *deferralFixture {
	t.Helper()
	df := &deferralFixture{model: &deferralLLM{
		responses: []*llm.Response{{
			ToolCalls:    []llm.ToolCall{{ID: "call-1", Name: lookupTool().Name, Arguments: `{}`}},
			FinishReason: "tool_calls",
			Usage:        llm.Usage{TotalTokens: 7},
		}},
		deferAt: deferAt,
	}}
	base := []Option{
		WithLLM(df.model),
		WithTool(lookupTool(), func(context.Context, cortex.Invocation) (string, error) {
			df.lookups.Add(1)
			return "found", nil
		}),
	}
	df.continueFixture = buildContinueFixture(ctx, t, st, nil, append(base, opts...)...)
	df.overrides = &RunOverrides{MaxSteps: 5}
	return df
}

// straightRun is the same run with nothing deferred: what a deferred run
// must add up to once it is continued.
func straightRun(t *testing.T) (*run.Run, *deferralFixture) {
	t.Helper()
	f := newDeferralFixture(t, nil)
	done, err := f.e.RunAgent(as(f.ctx, alice), "assistant", "clean up", f.overrides)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)
	return done, f
}

func TestDeferredModelCall_isTheSentinel(t *testing.T) {
	err := fmt.Errorf("meter: %w", cortex.DeferredModelCall{Kind: "credits", Reason: "out of credits"})
	require.ErrorIs(t, err, cortex.ErrDeferModelCall)
	var deferred cortex.DeferredModelCall
	require.ErrorAs(t, err, &deferred)
	require.Equal(t, "credits", deferred.Kind)
	require.Contains(t, err.Error(), "out of credits")
}

func TestModelCallDeferral_suspendsBeforeTheCall(t *testing.T) {
	f := newDeferralFixture(t, map[int]error{2: creditsDeferral()})
	paused, cp := f.pause(t)
	require.NotNil(t, cp, "a deferral opened no checkpoint")

	require.Equal(t, 2, f.model.calls(), "the model saw a call after the deferral")
	require.EqualValues(t, 1, f.lookups.Load(), "the tool did not run exactly once")

	require.Equal(t, checkpoint.KindDeferredModelCall, cp.Metadata[checkpoint.MetadataKind])
	require.Equal(t, "credits", cp.Metadata[checkpoint.MetadataDeferralKind])
	require.Equal(t, "The workspace is out of AI credits.", cp.Reason)
	require.Equal(t, 1, cp.StepIndex, "the checkpoint names a step other than the deferred one")

	susp, err := f.st.GetSuspension(f.ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, suspension.ReasonDeferredModelCall, susp.Reason)
	require.Empty(t, susp.Pending)
	require.Equal(t, 1, susp.Cont.StepIndex, "the run would not continue at the deferred call")
	require.NotNil(t, susp.ExpiresAt, "a deferral ignored the engine's suspension TTL")

	// Only the call that ran is counted.
	require.Equal(t, 7, paused.TokensUsed)
	require.Equal(t, 1, paused.StepCount)
	require.Equal(t, 1, paused.Usage.Steps)
}

func TestModelCallDeferral_continueRetriesTheSameCall(t *testing.T) {
	straight, sf := straightRun(t)

	f := newDeferralFixture(t, map[int]error{2: creditsDeferral()})
	paused, cp := f.pause(t)
	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
	require.Equal(t, 2, f.model.calls(), "deciding called the model")
	require.Equal(t, run.StateReady, f.runState(t, paused.ID).State)

	done, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)
	require.Equal(t, "done", done.Output)

	require.Equal(t, 3, f.model.calls())
	require.Equal(t, f.model.request(t, 1), f.model.request(t, 2), "the continued call is not the deferred one")
	require.Equal(t, sf.model.request(t, 1), f.model.request(t, 2), "the continued call differs from a straight run's")
	require.Equal(t, sf.lookups.Load(), f.lookups.Load(), "the tool ran a different number of times than in a straight run")
	require.Equal(t, straight.TokensUsed, done.TokensUsed, "the deferred call was counted")
	require.Equal(t, straight.StepCount, done.StepCount)
	require.Equal(t, straight.Usage.Steps, done.Usage.Steps)

	decided := f.checkpointState(t, cp.ID)
	require.NotEqual(t, checkpointStatePending, decided.State)
	require.NotNil(t, decided.Decision)
	require.True(t, decided.Decision.Approved)
}

func TestModelCallDeferral_firstCallOfTheRun(t *testing.T) {
	f := newDeferralFixture(t, map[int]error{1: fmt.Errorf("meter: %w", creditsDeferral())})
	paused, cp := f.pause(t)
	require.NotNil(t, cp)
	require.Equal(t, 1, f.model.calls())
	require.Zero(t, f.lookups.Load())
	require.Zero(t, paused.TokensUsed)
	require.Zero(t, paused.StepCount)
	require.Zero(t, cp.StepIndex)

	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
	done, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)
	require.Equal(t, 3, f.model.calls())
	require.Equal(t, f.model.request(t, 0), f.model.request(t, 1), "the continued call is not the deferred one")
	require.EqualValues(t, 1, f.lookups.Load())
	require.Equal(t, 14, done.TokensUsed)
}

func TestModelCallDeferral_resolveCheckpointContinuesTheRun(t *testing.T) {
	f := newDeferralFixture(t, map[int]error{2: creditsDeferral()})
	paused, cp := f.pause(t)
	require.NoError(t, f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
	done := f.runState(t, paused.ID)
	require.Equal(t, run.StateCompleted, done.State)
	require.Equal(t, 3, f.model.calls())
	require.EqualValues(t, 1, f.lookups.Load())
}

func TestModelCallDeferral_deferredAgainOnContinue(t *testing.T) {
	f := newDeferralFixture(t, map[int]error{2: creditsDeferral(), 3: creditsDeferral()})
	paused, cp := f.pause(t)
	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
	again, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, again.State)

	cps, err := f.e.ListPendingCheckpoints(f.ctx, &checkpoint.ListFilter{RunID: paused.ID.String()})
	require.NoError(t, err)
	require.Len(t, cps, 1)
	require.NotEqual(t, cp.ID, cps[0].ID)
	require.Equal(t, checkpoint.KindDeferredModelCall, cps[0].Metadata[checkpoint.MetadataKind])

	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cps[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
	done, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)
	require.EqualValues(t, 1, f.lookups.Load())
	require.Equal(t, 14, done.TokensUsed)
}

func TestModelCallDeferral_rejectionStopsTheRun(t *testing.T) {
	for name, opts := range map[string][]Option{
		"rejections fail runs":            nil,
		"rejections go back to the model": {WithRejectionsReturnToModel()},
	} {
		t.Run(name, func(t *testing.T) {
			f := newDeferralFixture(t, map[int]error{2: creditsDeferral()}, opts...)
			paused, cp := f.pause(t)
			require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{DecidedBy: "bob", Reason: "Cancelled by Bob."}))

			stopped := f.runState(t, paused.ID)
			require.Equal(t, run.StateCancelled, stopped.State)
			require.Equal(t, run.StopCancelled, stopped.StopReason)
			require.Contains(t, stopped.Error, "Cancelled by Bob.")
			require.Equal(t, 2, f.model.calls())
			require.Equal(t, 1, f.ended.of(paused.ID))

			decided := f.checkpointState(t, cp.ID)
			require.NotNil(t, decided.Decision)
			require.False(t, decided.Decision.Approved)
			require.Equal(t, "Cancelled by Bob.", decided.Decision.Reason)

			_, err := f.st.GetSuspension(f.ctx, paused.ID)
			require.ErrorIs(t, err, cortex.ErrNotSuspended)
			_, err = f.e.ContinueRun(f.ctx, paused.ID)
			require.ErrorIs(t, err, cortex.ErrRunAlreadyDone)
		})
	}
}

func TestModelCallDeferral_refusesAnythingButAPlainDecision(t *testing.T) {
	cases := map[string]checkpoint.Decision{
		"per-call decisions": {Calls: map[string]checkpoint.CallDecision{"call-1": {Approved: true}}},
		"amended arguments":  {Approved: true, AmendedArguments: []byte(`{"to":"b"}`)},
		"per-call rejection": {Calls: map[string]checkpoint.CallDecision{"call-1": {Reason: "no"}}},
	}
	for name, d := range cases {
		t.Run(name, func(t *testing.T) {
			f := newDeferralFixture(t, map[int]error{2: creditsDeferral()})
			paused, cp := f.pause(t)
			d.DecidedBy = "bob"
			require.ErrorIs(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, d), cortex.ErrInvalidDecision)
			require.ErrorIs(t, f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, d), cortex.ErrInvalidDecision)
			require.Equal(t, checkpointStatePending, f.checkpointState(t, cp.ID).State)
			require.Equal(t, run.StatePaused, f.runState(t, paused.ID).State)
			require.Equal(t, 2, f.model.calls())
		})
	}
}

func TestModelCallDeferral_onlyADecisionContinuesIt(t *testing.T) {
	f := newDeferralFixture(t, map[int]error{2: creditsDeferral()})
	paused, _ := f.pause(t)

	_, err := f.e.Resume(f.ctx, paused.ID, ResumeInput{})
	require.ErrorIs(t, err, cortex.ErrDeferModelCall)
	require.ErrorIs(t, f.e.ProvideResults(f.ctx, paused.ID, ResumeInput{}), cortex.ErrDeferModelCall)
	_, err = f.e.ContinueRun(f.ctx, paused.ID)
	require.ErrorIs(t, err, cortex.ErrRunNotContinuable)

	require.Equal(t, run.StatePaused, f.runState(t, paused.ID).State)
	require.Equal(t, 2, f.model.calls())
}

func TestModelCallDeferral_streamingRun(t *testing.T) {
	for name, onNext := range map[string]bool{"when the stream opens": false, "on the first chunk": true} {
		t.Run(name, func(t *testing.T) {
			f := newDeferralFixture(t, map[int]error{2: creditsDeferral()})
			f.model.onNext = onNext

			events := make(chan StreamEvent, 64)
			require.NoError(t, f.e.StreamAgent(as(f.ctx, alice), "assistant", "clean up", f.overrides, events))
			last := lastEvent(events)
			require.Equal(t, EventSuspended, last.Type, "the stream ended with %v", last.Data)
			require.Equal(t, string(suspension.ReasonDeferredModelCall), last.Data["reason"])
			require.Equal(t, 2, f.model.calls())
			require.EqualValues(t, 1, f.lookups.Load())

			runs, err := f.e.ListRuns(f.ctx, &run.ListFilter{Limit: 5})
			require.NoError(t, err)
			require.Len(t, runs, 1)
			paused := runs[0]
			require.Equal(t, run.StatePaused, paused.State)
			require.Equal(t, 7, paused.TokensUsed)
			cps, err := f.e.ListPendingCheckpoints(f.ctx, &checkpoint.ListFilter{RunID: paused.ID.String()})
			require.NoError(t, err)
			require.Len(t, cps, 1)
			require.Equal(t, checkpoint.KindDeferredModelCall, cps[0].Metadata[checkpoint.MetadataKind])

			require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cps[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
			continued := make(chan StreamEvent, 64)
			require.NoError(t, f.e.ContinueRunStream(f.ctx, paused.ID, continued))
			require.Equal(t, EventDone, lastEvent(continued).Type)

			done := f.runState(t, paused.ID)
			require.Equal(t, run.StateCompleted, done.State)
			require.Equal(t, 3, f.model.calls())
			require.Equal(t, f.model.request(t, 1), f.model.request(t, 2))
			require.EqualValues(t, 1, f.lookups.Load())
			require.Equal(t, 14, done.TokensUsed)
		})
	}
}

// lastEvent drains events and returns the last one.
func lastEvent(events <-chan StreamEvent) StreamEvent {
	var last StreamEvent
	for ev := range events {
		last = ev
	}
	return last
}

// A model error that is not a deferral still fails the run.
func TestModelCallDeferral_otherErrorsStillFail(t *testing.T) {
	f := newDeferralFixture(t, map[int]error{2: errors.New("provider down")})
	_, err := f.e.RunAgent(as(f.ctx, alice), "assistant", "clean up", f.overrides)
	require.Error(t, err)
	require.NotErrorIs(t, err, cortex.ErrDeferModelCall)
	runs, listErr := f.e.ListRuns(f.ctx, &run.ListFilter{Limit: 5})
	require.NoError(t, listErr)
	require.Len(t, runs, 1)
	require.Equal(t, run.StateFailed, runs[0].State)
}

// A store that cannot hold ready runs resolves a deferral in place: an
// approval makes the call on the caller's goroutine, a rejection stops the
// run, and anything but a plain decision is refused.
func TestModelCallDeferral_resolvedInPlace(t *testing.T) {
	inPlace := func(t *testing.T) *deferralFixture {
		ctx := cortex.WithScope(context.Background(), approvalScope())
		return deferralFixtureOn(ctx, t, hiddenContinuationStore{newApprovalStore(ctx, t)}, map[int]error{2: creditsDeferral()})
	}

	t.Run("approved", func(t *testing.T) {
		f := inPlace(t)
		paused, cp := f.pause(t)
		require.NoError(t, f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
		require.Equal(t, run.StateCompleted, f.runState(t, paused.ID).State)
		require.Equal(t, 3, f.model.calls())
		require.Equal(t, f.model.request(t, 1), f.model.request(t, 2))
		require.EqualValues(t, 1, f.lookups.Load())
		require.True(t, f.checkpointState(t, cp.ID).Decision.Approved)
	})
	t.Run("rejected", func(t *testing.T) {
		f := inPlace(t)
		paused, cp := f.pause(t)
		require.NoError(t, f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{DecidedBy: "bob", Reason: "Cancelled by Bob."}))
		stopped := f.runState(t, paused.ID)
		require.Equal(t, run.StateCancelled, stopped.State)
		require.Contains(t, stopped.Error, "Cancelled by Bob.")
		require.Equal(t, 2, f.model.calls())
		require.False(t, f.checkpointState(t, cp.ID).Decision.Approved)
	})
	t.Run("amended", func(t *testing.T) {
		f := inPlace(t)
		paused, cp := f.pause(t)
		err := f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob", AmendedArguments: []byte(`{"to":"b"}`)})
		require.ErrorIs(t, err, cortex.ErrInvalidDecision)
		require.Equal(t, run.StatePaused, f.runState(t, paused.ID).State)
		require.Equal(t, checkpointStatePending, f.checkpointState(t, cp.ID).State)
	})
}

// An empty allowlist and no allowlist freeze to the same tools: a run
// started with Tools set to an empty slice can be continued, although the
// continuation stores no tools at all.
func TestModelCallDeferral_continuesARunWithAnEmptyToolList(t *testing.T) {
	f := newDeferralFixture(t, map[int]error{1: creditsDeferral()})
	f.model.responses = nil
	f.overrides = &RunOverrides{MaxSteps: 5, Tools: []string{}, ToolsRestricted: true}
	paused, cp := f.pause(t)
	require.NotNil(t, cp)

	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
	done, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)
	require.Equal(t, "done", done.Output)
	require.Zero(t, f.lookups.Load())
}
