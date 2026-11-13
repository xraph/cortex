package engine

import (
	"context"
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
)

func (f *continueFixture) seenArguments() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.args)
}

func newAmendFixture(t *testing.T, opts ...Option) *continueFixture {
	t.Helper()
	ctx := cortexScopeContext()
	return buildContinueFixture(ctx, t, newApprovalStore(ctx, t), []llm.ToolCall{{ID: "call-1", Name: gatedTool().Name, Arguments: `{"to":"a"}`}}, opts...)
}

func cortexScopeContext() context.Context {
	return cortex.WithScope(context.Background(), approvalScope())
}

func TestContinueRun_amendedArgumentsReplaceTheModelsArguments(t *testing.T) {
	f := newAmendFixture(t)
	paused, cp := f.pause(t)

	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{
		Approved:         true,
		DecidedBy:        "bob",
		AmendedArguments: json.RawMessage(`{"to":"b"}`),
	}))
	done, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)

	require.Equal(t, []string{`{"to":"b"}`}, f.seenArguments(), "the tool must run once, with the amended arguments")

	// The model sees one result for the call, and its own history shows
	// the arguments that ran.
	messages := f.model.lastRequest()
	results := 0
	for _, m := range messages {
		if m.Role == "tool" && m.ToolCallID == "call-1" {
			results++
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "call-1" {
				require.JSONEq(t, `{"to":"b"}`, tc.Arguments)
			}
		}
	}
	require.Equal(t, 1, results)

	// The decision keeps the edit, and the trace says who made it.
	require.JSONEq(t, `{"to":"b"}`, string(f.checkpointState(t, cp.ID).Decision.AmendedArguments))
	steps, err := f.e.ListSteps(f.ctx, paused.ID)
	require.NoError(t, err)
	found := false
	for _, step := range steps {
		calls, listErr := f.e.ListToolCalls(f.ctx, step.ID)
		require.NoError(t, listErr)
		for _, call := range calls {
			if call.ToolName == gatedTool().Name {
				found = true
				require.JSONEq(t, `{"to":"b"}`, call.Arguments)
				require.Equal(t, true, call.Metadata["arguments_amended"])
				require.Equal(t, "bob", call.Metadata["amended_by"])
			}
		}
	}
	require.True(t, found, "the approved call left no tool call row")
}

func TestDecideCheckpoint_amendedArgumentsMustBeAnObject(t *testing.T) {
	for _, bad := range []string{`[1]`, `"x"`, `null`, `{`} {
		t.Run(bad, func(t *testing.T) {
			f := newAmendFixture(t)
			paused, cp := f.pause(t)

			err := f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{
				Approved:         true,
				AmendedArguments: json.RawMessage(bad),
			})
			require.ErrorIs(t, err, cortex.ErrInvalidDecision)

			// The refusal leaves the checkpoint open and the run paused.
			require.Equal(t, "pending", f.checkpointState(t, cp.ID).State)
			require.Equal(t, run.StatePaused, f.runState(t, paused.ID).State)
		})
	}
}

func TestDecideCheckpoint_amendedArgumentsNeedOnePendingCall(t *testing.T) {
	ctx := cortexScopeContext()
	f := buildContinueFixture(ctx, t, newApprovalStore(ctx, t), []llm.ToolCall{
		{ID: "call-1", Name: gatedTool().Name, Arguments: `{"to":"a"}`},
		{ID: "call-2", Name: gatedTool().Name, Arguments: `{"to":"c"}`},
	})
	paused, cp := f.pause(t)

	err := f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, AmendedArguments: json.RawMessage(`{"to":"b"}`)})
	require.ErrorIs(t, err, cortex.ErrInvalidDecision)
	require.Equal(t, run.StatePaused, f.runState(t, paused.ID).State)
}

func TestContinueRun_rejectedIgnoresAmendedArguments(t *testing.T) {
	f := newAmendFixture(t, WithRejectionsReturnToModel())
	paused, cp := f.pause(t)

	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{
		DecidedBy:        "bob",
		Reason:           "no",
		AmendedArguments: json.RawMessage(`{"to":"b"}`),
	}))
	_, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)

	require.Zero(t, f.ran.Load(), "a rejected call must never run")
	content, ok := toolMessage(f.model.lastRequest())
	require.True(t, ok)
	require.Contains(t, content, "rejected by bob: no")
	require.Empty(t, f.checkpointState(t, cp.ID).Decision.AmendedArguments, "a rejection must not record an edit")
}

func newTwoCallFixture(t *testing.T) *continueFixture {
	t.Helper()
	ctx := cortexScopeContext()
	return buildContinueFixture(ctx, t, newApprovalStore(ctx, t), []llm.ToolCall{
		{ID: "call-1", Name: gatedTool().Name, Arguments: `{"to":"a"}`},
		{ID: "call-2", Name: gatedTool().Name, Arguments: `{"to":"c"}`},
	})
}

func TestContinueRun_perCallDecisionsApproveAmendAndReject(t *testing.T) {
	f := newTwoCallFixture(t)
	paused, cp := f.pause(t)

	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{
		DecidedBy: "bob",
		Calls: map[string]checkpoint.CallDecision{
			"call-1": {Approved: true, AmendedArguments: json.RawMessage(`{"to":"b"}`)},
			"call-2": {Reason: "wrong recipient"},
		},
	}))
	done, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)

	require.Equal(t, []string{`{"to":"b"}`}, f.seenArguments(), "only the approved call runs, with the amended arguments")
	var first, second string
	for _, m := range f.model.lastRequest() {
		if m.Role != "tool" {
			continue
		}
		switch m.ToolCallID {
		case "call-1":
			first = m.Content
		case "call-2":
			second = m.Content
		}
	}
	require.Equal(t, "deleted", first)
	require.Contains(t, second, "rejected by bob: wrong recipient")
	require.Equal(t, 1, countEvent(f.tools.snapshot(), "completed"))
	require.Equal(t, 1, countEvent(f.tools.snapshot(), "denied"))
	require.Zero(t, countEvent(f.tools.snapshot(), "failed"))

	decided := f.checkpointState(t, cp.ID).Decision
	require.True(t, decided.Approved, "a decision with any approved call is approved")
	require.Len(t, decided.Calls, 2)
	require.JSONEq(t, `{"to":"b"}`, string(decided.Calls["call-1"].AmendedArguments))
	require.Equal(t, "wrong recipient", decided.Calls["call-2"].Reason)
}

func TestDecideCheckpoint_perCallDecisionsMustNameEveryPendingCall(t *testing.T) {
	cases := map[string]map[string]checkpoint.CallDecision{
		"missing call":  {"call-1": {Approved: true}},
		"unknown call":  {"call-1": {Approved: true}, "call-2": {Approved: true}, "call-9": {Approved: true}},
		"wrong call id": {"call-1": {Approved: true}, "call-9": {Approved: true}},
		"amend not obj": {"call-1": {Approved: true, AmendedArguments: json.RawMessage(`[1]`)}, "call-2": {Approved: true}},
	}
	for name, calls := range cases {
		t.Run(name, func(t *testing.T) {
			f := newTwoCallFixture(t)
			paused, cp := f.pause(t)

			err := f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Calls: calls})
			require.ErrorIs(t, err, cortex.ErrInvalidDecision)
			require.Equal(t, "pending", f.checkpointState(t, cp.ID).State)
			require.Equal(t, run.StatePaused, f.runState(t, paused.ID).State)
		})
	}
}

func TestDecideCheckpoint_perCallDecisionsRefuseTopLevelAmendment(t *testing.T) {
	f := newTwoCallFixture(t)
	_, cp := f.pause(t)
	err := f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{
		AmendedArguments: json.RawMessage(`{"to":"b"}`),
		Calls:            map[string]checkpoint.CallDecision{"call-1": {Approved: true}, "call-2": {Approved: true}},
	})
	require.ErrorIs(t, err, cortex.ErrInvalidDecision)
}

func TestDecideCheckpoint_perCallDecisionsAllRejectedFailsRunWithoutOption(t *testing.T) {
	f := newTwoCallFixture(t)
	paused, cp := f.pause(t)
	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{
		DecidedBy: "bob",
		Calls:     map[string]checkpoint.CallDecision{"call-1": {Reason: "no"}, "call-2": {Reason: "no"}},
	}))
	require.Equal(t, run.StateFailed, f.runState(t, paused.ID).State)
	require.Zero(t, f.ran.Load())
	require.False(t, f.checkpointState(t, cp.ID).Decision.Approved)
}

func TestResolveCheckpoint_perCallDecisionsContinueTheRun(t *testing.T) {
	f := newTwoCallFixture(t)
	paused, cp := f.pause(t)
	require.NoError(t, f.e.ResolveCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{
		DecidedBy: "bob",
		Calls: map[string]checkpoint.CallDecision{
			"call-1": {Reason: "no"},
			"call-2": {Approved: true, AmendedArguments: json.RawMessage(`{"to":"z"}`)},
		},
	}))
	require.Equal(t, run.StateCompleted, f.runState(t, paused.ID).State)
	require.Equal(t, []string{`{"to":"z"}`}, f.seenArguments())
}
