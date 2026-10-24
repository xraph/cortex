package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
)

type recoveryProviderProbe func(context.Context, execution.RunCallRequest) (execution.ProviderCallReceipt, error)

func (f recoveryProviderProbe) ReconcileProviderCall(ctx context.Context, r execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
	return f(ctx, r)
}

type recoveryProviderFence func(context.Context, execution.RunCallRequest, func() error) error

func (f recoveryProviderFence) WithProviderCallFence(ctx context.Context, r execution.RunCallRequest, commit func() error) error {
	return f(ctx, r, commit)
}

func TestProviderRecoveryAfterWorkerProcessCrash(t *testing.T) {
	for _, boundary := range []string{"provider_pending", "completed"} {
		t.Run(boundary, func(t *testing.T) {
			m := killTaskWorkerAtBoundary(t, boundary)
			s := openSQLiteStoreForTest(t, m.Database)
			ctx := cortex.WithScope(t.Context(), m.Claim.Scope)
			calls, err := s.ListRunCalls(ctx, execution.RunCallListRequest{RunID: m.Dispatch.RunID, Limit: 10})
			require.NoError(t, err)
			require.Len(t, calls, 1)
			original := calls[0]
			_, digest, err := original.Request.Freeze()
			require.NoError(t, err)
			resultDigest := evidence.Digest([]byte("fixture terminal result after lost reply"))
			if original.Completion != nil {
				resultDigest = original.Completion.ResultDigest
			}
			expected := execution.ProviderCallReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.ProviderCallSucceeded, Provider: "fixture", AdapterRevision: "v1", AccountRef: "original-fixture-account", OperationID: original.Request.ID.String(), ProofRef: "retained-fixture-proof", ProofDigest: evidence.Digest([]byte("fixture terminal proof")), ObservedAt: time.Now().UTC(), ResultDigest: resultDigest, Usage: execution.ReservationUsage{Known: execution.KnownTokens, Actual: execution.Resources{Tokens: 7}}}
			queries, authenticated := 0, 0
			config := execution.ProviderRecoveryConfig{
				Store: s,
				Adapter: recoveryProviderProbe(func(_ context.Context, r execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
					require.Equal(t, original.Request, r)
					queries++
					return expected, nil
				}),
				Authorize: func(_ context.Context, r execution.RunCallRequest) error {
					require.Equal(t, original.Request, r)
					return nil
				},
				Authenticate: func(_ context.Context, r execution.RunCallRequest, receipt execution.ProviderCallReceipt) error {
					// This is the host authority fixture, not a live provider attestation.
					require.Equal(t, original.Request, r)
					require.Equal(t, expected, receipt)
					authenticated++
					return nil
				},
				Fence: recoveryProviderFence(func(_ context.Context, _ execution.RunCallRequest, commit func() error) error { return commit() }),
			}
			reconciler, err := execution.NewProviderReconciler(config)
			require.NoError(t, err)
			beforeRun, err := s.GetRun(ctx, m.Dispatch.RunID)
			require.NoError(t, err)
			beforeBudget, err := s.GetBudgetSnapshot(ctx, m.Claim.PlanID)
			require.NoError(t, err)
			receipt, err := reconciler.Reconcile(ctx, original.Request)
			require.NoError(t, err)
			require.Equal(t, expected, receipt)
			// Recreate the service and storage connection, then reauthenticate saved proof.
			reopened := openSQLiteStoreForTest(t, m.Database)
			config.Store = reopened
			reconciler, err = execution.NewProviderReconciler(config)
			require.NoError(t, err)
			receipt, err = reconciler.Reconcile(ctx, original.Request)
			require.NoError(t, err)
			require.Equal(t, expected, receipt)
			require.Equal(t, 1, queries)
			require.Equal(t, 2, authenticated)
			after, err := reopened.GetRunCall(ctx, original.Request.RunID, original.Request.OperationDigest)
			require.NoError(t, err)
			require.Equal(t, original, after, "provider proof must not invent a worker return")
			claim, err := reopened.GetTaskClaim(ctx, m.Claim.PlanID, m.Claim.TaskID)
			require.NoError(t, err)
			require.Equal(t, m.Claim, claim)
			afterRun, err := reopened.GetRun(ctx, m.Dispatch.RunID)
			require.NoError(t, err)
			require.Equal(t, beforeRun, afterRun)
			afterBudget, err := reopened.GetBudgetSnapshot(ctx, m.Claim.PlanID)
			require.NoError(t, err)
			require.Equal(t, beforeBudget, afterBudget)
			fresh, err := reopened.BeginRunCall(ctx, original.Request)
			require.NoError(t, err)
			require.False(t, fresh)
		})
	}
}
