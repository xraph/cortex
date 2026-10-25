package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

type activationClient struct {
	outcomeClient
	calls int
}

func (c *activationClient) Complete(ctx context.Context, request *llm.Request) (*llm.Response, error) {
	c.calls++
	return c.outcomeClient.Complete(ctx, request)
}

func TestPreparedActivationUsesReservedIdentityAndCapturedArtifact(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), scopeA())
	s := newApprovalStore(ctx, t)
	client := &activationClient{outcomeClient: outcomeClient{response: &llm.Response{Content: "answer", FinishReason: "stop"}}}
	e, err := New(WithStore(s), WithLLM(client))
	require.NoError(t, err)
	ag, err := s.GetByName(ctx, "assistant")
	require.NoError(t, err)
	childID := id.NewAgentRunID()
	p, err := e.PrepareAgent(ctx, ag.ID, &RunOverrides{SystemPrompt: "captured"})
	require.NoError(t, err)
	digest := p.Digest()
	ctx, draft, err := e.prepareRun(ctx, p, "work", PreparedRunOptions{}, childID)
	require.NoError(t, err)
	before, err := s.ListRuns(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, before, "preflight created the child before team linkage")
	require.Zero(t, client.calls)
	replacement, err := e.PrepareAgent(ctx, ag.ID, &RunOverrides{SystemPrompt: "replacement"})
	require.NoError(t, err)
	data, err := json.Marshal(replacement)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(data, p))
	ctx, rz, err := e.activatePrepared(ctx, draft)
	require.NoError(t, err)
	require.Equal(t, childID, rz.run.ID)
	require.Equal(t, digest, rz.run.Provenance.PreparedDigest)
	require.Equal(t, digest, rz.state.prepared.Digest(), "caller replacement changed continuation artifact")
	require.Equal(t, "captured", rz.state.systemPrompt)
	require.Zero(t, client.calls, "activation invoked a provider before entering the loop")
	finished, err := e.continueReAct(ctx, rz.ag, rz.cfg, rz.run, rz.state, rz.startedAt)
	require.NoError(t, err)
	require.Equal(t, childID, finished.ID)
	require.Equal(t, run.StateCompleted, finished.State)
	require.Equal(t, 1, client.calls)
	_, duplicate, err := e.activatePrepared(ctx, draft)
	require.Error(t, err)
	require.Nil(t, duplicate, "existing child must not enter the loop again")
	var startup *PreparedStartError
	require.ErrorAs(t, err, &startup)
	require.Equal(t, childID, startup.RunIdentity())
	stored, err := s.GetRun(ctx, childID)
	require.NoError(t, err)
	require.Equal(t, run.StateCompleted, stored.State)
	require.Equal(t, "answer", stored.Output)
}

func TestPreparedActivationCancellationDoesNotCreateRun(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), scopeA())
	s := scopespy.New()
	e, err := New(WithStore(s), WithLLM(&outcomeClient{}))
	require.NoError(t, err)
	p, err := e.PrepareAgent(ctx, id.NewAgentID(), nil)
	require.NoError(t, err)
	ctx, draft, err := e.prepareRun(ctx, p, "work", PreparedRunOptions{}, id.NewAgentRunID())
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	_, started, err := e.activatePrepared(ctx, draft)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, started)
	require.Empty(t, s.Runs())
}

func TestPreparedDraftRejectsInvalidReservedID(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), scopeA())
	s := scopespy.New()
	e, err := New(WithStore(s), WithLLM(&outcomeClient{}))
	require.NoError(t, err)
	p, err := e.PrepareAgent(ctx, id.NewAgentID(), nil)
	require.NoError(t, err)
	for _, key := range []id.AgentRunID{{}, id.NewAgentID()} {
		_, draft, draftErr := e.prepareRun(ctx, p, "work", PreparedRunOptions{}, key)
		require.ErrorIs(t, draftErr, cortex.ErrInvalidPrepared)
		require.Nil(t, draft)
	}
	require.Empty(t, s.Runs())
}
