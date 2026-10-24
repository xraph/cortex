package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

type scanProvider func(context.Context, execution.RunCallRequest) (execution.ProviderCallReceipt, error)

func (f scanProvider) ReconcileProviderCall(ctx context.Context, r execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
	return f(ctx, r)
}

type scanProviderFence struct{}

func (scanProviderFence) WithProviderCallFence(_ context.Context, _ execution.RunCallRequest, next func() error) error {
	return next()
}

func testProviderRecoveryScan(t *testing.T, newStore func(*testing.T) store.Store) {
	storage := newStore(t)
	receipts, ok := storage.(execution.ProviderCallReceiptStore)
	require.True(t, ok)
	seals, ok := storage.(execution.RunCallSealStore)
	require.True(t, ok)
	original := storedRunCall()
	ctx := cortex.WithScope(t.Context(), original.Scope)
	calls := make([]execution.RunCallRequest, 0, 5)
	for _, name := range []string{"complete", "stream", "tool", "later", "failed"} {
		r := original
		r.ID, r.OperationDigest = id.New(execution.PrefixRunCall), evidence.Digest([]byte(name))
		switch name {
		case "stream":
			r.Kind = execution.RunCallStream
		case "tool":
			r.Kind = execution.RunCallTool
		}
		fresh, err := receipts.BeginRunCall(ctx, r)
		require.NoError(t, err)
		require.True(t, fresh)
		calls = append(calls, r)
	}
	seal, err := seals.SealRunCalls(ctx, execution.RunCallSealRequest{SchemaVersion: 1, Scope: original.Scope, RunID: original.RunID, Actor: "fixture-recovery", Reason: "scan retained calls"})
	require.NoError(t, err)
	unknown := true
	probes, proofs := 0, 0
	config := execution.ProviderRecoveryConfig{
		Store: receipts,
		AuthorizeScan: func(_ context.Context, offered execution.RunCallSeal) error {
			require.Equal(t, seal, offered)
			return nil
		},
		Authorize: func(context.Context, execution.RunCallRequest) error { return nil },
		Authenticate: func(context.Context, execution.RunCallRequest, execution.ProviderCallReceipt) error {
			proofs++
			return nil
		},
		Fence: scanProviderFence{},
		Adapter: scanProvider(func(_ context.Context, r execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
			probes++
			if r.OperationDigest == calls[3].OperationDigest && unknown {
				return execution.ProviderCallReceipt{}, execution.ErrProviderCallUncertain
			}
			_, digest, freezeErr := r.Freeze()
			require.NoError(t, freezeErr)
			receipt := execution.ProviderCallReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.ProviderCallSucceeded, Provider: "fixture", AdapterRevision: "v1", AccountRef: "original", OperationID: r.ID.String(), ProofRef: "fixture-proof", ProofDigest: evidence.Digest([]byte("trusted fixture")), ObservedAt: time.Now().UTC(), ResultDigest: evidence.Digest([]byte("fixture result"))}
			if r.OperationDigest == calls[4].OperationDigest {
				receipt.Outcome = execution.ProviderCallFailed
				receipt.ResultDigest = ""
			}
			return receipt, nil
		}),
	}
	service, err := execution.NewProviderReconciler(config)
	require.NoError(t, err)
	scan := func() execution.ProviderRecoveryPage {
		request := execution.ProviderRecoveryPageRequest{Seal: seal, Limit: 2}
		var total execution.ProviderRecoveryPage
		for pages := 0; pages < 4; pages++ {
			page, scanErr := service.ReconcilePage(ctx, request)
			require.NoError(t, scanErr)
			total.Scanned += page.Scanned
			total.Terminal += page.Terminal
			total.Unresolved += page.Unresolved
			total.ToolCalls += page.ToolCalls
			if page.NextAfter == "" {
				return total
			}
			require.Greater(t, page.NextAfter, request.After)
			request.After = page.NextAfter
		}
		t.Fatal("bounded journal did not terminate")
		return total
	}
	require.Equal(t, execution.ProviderRecoveryPage{Scanned: 5, Terminal: 3, Unresolved: 1, ToolCalls: 1}, scan())
	require.Equal(t, 4, probes)
	require.Equal(t, 3, proofs)
	unknown = false
	// Reconstruct the service to ensure the next pass uses durable receipts.
	service, err = execution.NewProviderReconciler(config)
	require.NoError(t, err)
	require.Equal(t, execution.ProviderRecoveryPage{Scanned: 5, Terminal: 4, ToolCalls: 1}, scan())
	require.Equal(t, 5, probes)
	require.Equal(t, 7, proofs)
	record, err := receipts.GetRunCall(ctx, original.RunID, calls[0].OperationDigest)
	require.NoError(t, err)
	require.Nil(t, record.Completion, "provider proof is separate from a worker return")
	_, digest, err := calls[0].Freeze()
	require.NoError(t, err)
	require.NoError(t, receipts.CompleteRunCall(ctx, calls[0], execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("contradictory late return"))}))
	page, err := service.ReconcilePage(ctx, execution.ProviderRecoveryPageRequest{Seal: seal, Limit: 10})
	require.ErrorIs(t, err, execution.ErrRunCallConflict)
	require.Empty(t, page)
	proof, err := receipts.GetProviderCallReceipt(ctx, calls[0])
	require.NoError(t, err)
	require.NotNil(t, proof)
	require.Equal(t, evidence.Digest([]byte("fixture result")), proof.ResultDigest, "contradiction preserves original proof")
}
