package coordination_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
)

type replyDiscovery func(context.Context, id.AgentRunID, int) ([]id.AgentRunID, error)

func (f replyDiscovery) ListAgentReplyRuns(ctx context.Context, after id.AgentRunID, limit int) ([]id.AgentRunID, error) {
	return f(ctx, after, limit)
}

func TestScopedAgentReplySourceRechecksAuthorityAndPages(t *testing.T) {
	scopes := []cortex.Scope{{Levels: []cortex.Level{{Key: "workspace", Value: "empty"}}}, {Levels: []cortex.Level{{Key: "workspace", Value: "work"}}}}
	key := id.NewAgentRunID()
	denied := errors.New("revoked after query")
	revoke, auth := false, 0
	source, err := coordination.NewScopedAgentReplySource(replyDiscovery(func(ctx context.Context, after id.AgentRunID, limit int) ([]id.AgentRunID, error) {
		require.Equal(t, 1, limit)
		if cortex.ScopeFromContext(ctx).Levels[0].Value == "empty" || !after.IsNil() {
			return nil, nil
		}
		return []id.AgentRunID{key}, nil
	}), scopes, func(context.Context, cortex.Scope) error {
		auth++
		if revoke && auth%2 == 0 {
			return denied
		}
		return nil
	})
	require.NoError(t, err)
	scopes[0].Levels[0].Value = "caller mutation"
	empty, err := source.ListAgentReplyRuns(t.Context(), coordination.AgentReplyListRequest{Limit: 1})
	require.NoError(t, err)
	require.Empty(t, empty.Runs)
	require.NotEmpty(t, empty.Next)
	page, err := source.ListAgentReplyRuns(t.Context(), coordination.AgentReplyListRequest{Limit: 1, After: empty.Next})
	require.NoError(t, err)
	require.Equal(t, key, page.Runs[0].ID)
	end, err := source.ListAgentReplyRuns(t.Context(), coordination.AgentReplyListRequest{Limit: 1, After: page.Next})
	require.NoError(t, err)
	require.Empty(t, end.Runs)
	require.Empty(t, end.Next)
	revoke = true
	page, err = source.ListAgentReplyRuns(t.Context(), coordination.AgentReplyListRequest{Limit: 1, After: empty.Next})
	require.ErrorIs(t, err, denied)
	require.Empty(t, page.Runs)
	for _, cursor := range []string{"bad", "-1:", "2:", "1:" + id.NewSuspensionID().String()} {
		_, err = source.ListAgentReplyRuns(t.Context(), coordination.AgentReplyListRequest{Limit: 1, After: cursor})
		require.Error(t, err)
	}
}
