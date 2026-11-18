package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
)

// plainLookupTool is an ungated tool: the fixture's authorizer escalates only
// the gated one, so a call to this one runs inside the agent loop.
func plainLookupTool() llm.Tool {
	return llm.Tool{Name: "lookup", Description: "test-only ungated tool"}
}

func newLookupFixture(t *testing.T, calls []llm.ToolCall, opts ...Option) *continueFixture {
	t.Helper()
	ctx := cortexScopeContext()
	opts = append(opts, WithTool(plainLookupTool(), func(context.Context, cortex.Invocation) (string, error) {
		return "found", nil
	}))
	return buildContinueFixture(ctx, t, newApprovalStore(ctx, t), calls, opts...)
}

// toolCallRows is every tool call row of a run, in step order.
func toolCallRows(t *testing.T, f *continueFixture, runID id.AgentRunID) []*run.ToolCall {
	t.Helper()
	steps, err := f.e.ListSteps(f.ctx, runID)
	require.NoError(t, err)
	var rows []*run.ToolCall
	for _, step := range steps {
		calls, err := f.e.ListToolCalls(f.ctx, step.ID)
		require.NoError(t, err)
		rows = append(rows, calls...)
	}
	return rows
}

// recordedCallIDs is the call id each named tool's rows carry.
func recordedCallIDs(t *testing.T, f *continueFixture, runID id.AgentRunID, toolName string) []any {
	t.Helper()
	var ids []any
	for _, row := range toolCallRows(t, f, runID) {
		if row.ToolName == toolName {
			ids = append(ids, row.Metadata[run.MetadataCallID])
		}
	}
	return ids
}

func twoLookups() []llm.ToolCall {
	return []llm.ToolCall{
		{ID: "call-a", Name: plainLookupTool().Name, Arguments: `{"q":"a"}`},
		{ID: "call-b", Name: plainLookupTool().Name, Arguments: `{"q":"b"}`},
	}
}

func TestRunAgent_recordsEachToolCallsID(t *testing.T) {
	f := newLookupFixture(t, twoLookups())
	done, err := f.e.RunAgent(as(f.ctx, alice), "assistant", "look up", nil)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)

	require.ElementsMatch(t, []any{"call-a", "call-b"}, recordedCallIDs(t, f, done.ID, plainLookupTool().Name))
}

func TestStreamAgent_recordsEachToolCallsID(t *testing.T) {
	f := newLookupFixture(t, twoLookups())
	events := make(chan StreamEvent, 64)
	require.NoError(t, f.e.StreamAgent(as(f.ctx, alice), "assistant", "look up", nil, events))
	for range events { //nolint:revive // drain the stream until the run ends
	}

	runs, err := f.e.ListRuns(f.ctx, &run.ListFilter{})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, run.StateCompleted, runs[0].State)

	require.ElementsMatch(t, []any{"call-a", "call-b"}, recordedCallIDs(t, f, runs[0].ID, plainLookupTool().Name))
}

func TestRunAgent_aCallWithNoIDLeavesNoCallIDKey(t *testing.T) {
	f := newLookupFixture(t, []llm.ToolCall{{Name: plainLookupTool().Name, Arguments: `{}`}})
	done, err := f.e.RunAgent(as(f.ctx, alice), "assistant", "look up", nil)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)

	rows := toolCallRows(t, f, done.ID)
	require.Len(t, rows, 1)
	require.Nil(t, rows[0].Metadata, "a call the model gave no id must not record an empty one")
}

func TestCallMetadata(t *testing.T) {
	public := &run.Run{}
	require.Nil(t, callMetadata(public, ""))
	require.Equal(t, map[string]any{run.MetadataCallID: "call-1"}, callMetadata(public, "call-1"))

	// A run that requires verification keeps the model's id private, as
	// it does the rest of what the model generated.
	private := &run.Run{Provenance: run.ExecutionProvenance{VerificationRequired: true}}
	require.Nil(t, callMetadata(private, "call-1"))
}

func TestContinueRun_approvedCallRecordsItsCallID(t *testing.T) {
	f := newAmendFixture(t)
	paused, cp := f.pause(t)
	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{Approved: true, DecidedBy: "bob"}))
	done, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, done.State)

	rows := toolCallRows(t, f, paused.ID)
	require.Len(t, rows, 1)
	require.Equal(t, map[string]any{run.MetadataCallID: "call-1"}, rows[0].Metadata, "an unamended call records only its id")
}

func TestContinueRun_rejectedCallRecordsItsCallID(t *testing.T) {
	f := newAmendFixture(t, WithRejectionsReturnToModel())
	paused, cp := f.pause(t)
	require.NoError(t, f.e.DecideCheckpoint(as(f.ctx, bob), cp.ID, checkpoint.Decision{
		DecidedBy:        "bob",
		Reason:           "no",
		AmendedArguments: json.RawMessage(`{"to":"b"}`),
	}))
	_, err := f.e.ContinueRun(f.ctx, paused.ID)
	require.NoError(t, err)

	rows := toolCallRows(t, f, paused.ID)
	require.Len(t, rows, 1)
	require.Equal(t, map[string]any{run.MetadataCallID: "call-1"}, rows[0].Metadata, "a rejection records the id and no edit")
}
