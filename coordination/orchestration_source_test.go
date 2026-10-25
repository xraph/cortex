package coordination_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

type orchestrationBudgetSource func(context.Context, orchestration.BudgetListRequest) ([]orchestration.BudgetPolicy, error)

func (f orchestrationBudgetSource) ListOrchestrationBudgets(ctx context.Context, r orchestration.BudgetListRequest) ([]orchestration.BudgetPolicy, error) {
	return f(ctx, r)
}

func TestScopedOrchestrationSourcePagesAndRechecksAuthority(t *testing.T) {
	scopes := []cortex.Scope{{Levels: []cortex.Level{{Key: "workspace", Value: "empty"}}}, {Levels: []cortex.Level{{Key: "workspace", Value: "populated"}}}}
	retained := scopes[1]
	allocation := execution.Resources{Tokens: 100, FanOut: 1}
	policy := orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: id.NewOrchestrationID(), PreparedDigest: evidence.Digest([]byte("team")), Scope: retained, Limits: allocation, Participants: map[string]execution.Resources{"worker": allocation}, Deadline: time.Now().Add(time.Hour), TokenOnly: true}
	calls, auth := 0, 0
	denied := errors.New("revoked")
	revoke := false
	source, err := coordination.NewScopedOrchestrationSource(orchestrationBudgetSource(func(ctx context.Context, r orchestration.BudgetListRequest) ([]orchestration.BudgetPolicy, error) {
		calls++
		if calls == 1 {
			require.Equal(t, "empty", cortex.ScopeFromContext(ctx).Levels[0].Value)
		}
		if cortex.ScopeFromContext(ctx).Canonical() != retained.Canonical() || !r.After.IsNil() {
			return nil, nil
		}
		return []orchestration.BudgetPolicy{policy}, nil
	}), scopes, func(context.Context, cortex.Scope) error {
		auth++
		if revoke && auth%2 == 0 {
			return denied
		}
		return nil
	})
	require.NoError(t, err)
	scopes[0].Levels[0].Value = "caller mutation"
	empty, err := source.ListOrchestrations(t.Context(), coordination.OrchestrationListRequest{Limit: 1})
	require.NoError(t, err)
	require.Empty(t, empty.Teams)
	require.NotEmpty(t, empty.Next)
	page, err := source.ListOrchestrations(t.Context(), coordination.OrchestrationListRequest{Limit: 1, After: empty.Next})
	require.NoError(t, err)
	require.Equal(t, []coordination.ScheduledOrchestration{{Scope: retained, ID: policy.ExecutionID}}, page.Teams)
	end, err := source.ListOrchestrations(t.Context(), coordination.OrchestrationListRequest{Limit: 1, After: page.Next})
	require.NoError(t, err)
	require.Empty(t, end.Teams)
	require.Empty(t, end.Next)
	require.Equal(t, 3, calls)
	revoke = true
	rejected, err := source.ListOrchestrations(t.Context(), coordination.OrchestrationListRequest{Limit: 1, After: empty.Next})
	require.ErrorIs(t, err, denied)
	require.Empty(t, rejected.Teams)
	for _, cursor := range []string{"bad", "-1:", "2:", "1:" + id.NewAgentRunID().String()} {
		_, err = source.ListOrchestrations(t.Context(), coordination.OrchestrationListRequest{Limit: 1, After: cursor})
		require.Error(t, err)
	}
}
