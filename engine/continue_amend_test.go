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
