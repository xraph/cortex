package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

func snapshotSeal(ctx context.Context, t *testing.T, s execution.RunCallSealStore, r execution.RunCallRequest) execution.RunCallSeal {
	t.Helper()
	seal, err := s.SealRunCalls(ctx, execution.RunCallSealRequest{SchemaVersion: 1, Scope: r.Scope, RunID: r.RunID, Actor: "recovery", Reason: "read exact evidence"})
	require.NoError(t, err)
	return seal
}

func snapshotProof(t *testing.T, r execution.RunCallRequest) execution.ProviderCallReceipt {
	t.Helper()
	_, digest, err := r.Freeze()
	require.NoError(t, err)
	return execution.ProviderCallReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: execution.ProviderCallSucceeded, Provider: "fixture", AdapterRevision: "1", AccountRef: "account", OperationID: "operation", ProofRef: "proof", ProofDigest: evidence.Digest([]byte("proof")), ObservedAt: time.Now().UTC(), ResultDigest: evidence.Digest([]byte("provider result"))}
}

func testRunRecoverySnapshot(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	s, ok := backend.(execution.RunRecoverySnapshotStore)
	require.True(t, ok)
	calls, ok := backend.(execution.RunCallSealStore)
	require.True(t, ok)
	effects, ok := backend.(execution.BoundEffectStore)
	require.True(t, ok)
	providers, ok := backend.(execution.ProviderCallReceiptStore)
	require.True(t, ok)
	r := storedBoundEffect()
	ctx := cortex.WithScope(t.Context(), r.Scope)
	_, err := calls.BeginRunCall(ctx, *r.RunCall)
	require.NoError(t, err)
	_, err = effects.BeginBoundEffect(ctx, r)
	require.NoError(t, err)
	model := snapshotModel(r)
	_, err = calls.BeginRunCall(ctx, model)
	require.NoError(t, err)
	legacy := storedEffect()
	legacy.Scope = r.Scope
	legacy.RunID = r.RunID
	_, err = effects.BeginEffect(ctx, legacy)
	require.NoError(t, err)
	seal := snapshotSeal(ctx, t, calls, model)
	initial, err := s.SnapshotRunRecovery(ctx, seal)
	require.NoError(t, err)
	require.NoError(t, initial.Validate())
	require.Len(t, initial.Calls, 2)
	require.Len(t, initial.Effects, 2)
	again, err := s.SnapshotRunRecovery(ctx, seal)
	require.NoError(t, err)
	require.Equal(t, initial, again)
	require.NoError(t, effects.CompleteEffect(ctx, r, storedEffectReceipt(t, r)))
	proof := snapshotProof(t, model)
	require.NoError(t, providers.PutProviderCallReceipt(ctx, model, proof))
	completion := execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: proof.RequestDigest, ResultDigest: evidence.Digest([]byte("contradictory local result"))}
	require.NoError(t, calls.CompleteRunCall(ctx, model, completion))
	late, err := s.SnapshotRunRecovery(ctx, seal)
	require.NoError(t, err)
	require.NoError(t, late.Validate())
	require.NotEqual(t, initial.Digest, late.Digest)
	for _, entry := range late.Calls {
		if entry.Call.Request.ID == model.ID {
			require.NotNil(t, entry.Provider)
			require.ErrorIs(t, entry.Provider.ValidateObservation(entry.Call), execution.ErrRunCallConflict)
		}
	}
	for _, entry := range late.Effects {
		if entry.Request.OperationKey == legacy.OperationKey {
			require.Nil(t, entry.Request.RunCall)
			require.Nil(t, entry.Receipt)
		} else {
			require.NotNil(t, entry.Receipt)
		}
	}
	late.Calls[0].Call.Request.Scope.Levels[0].Value = "mutated"
	reread, err := s.SnapshotRunRecovery(ctx, seal)
	require.NoError(t, err)
	require.NoError(t, reread.Validate())
	altered := seal
	altered.At = altered.At.Add(time.Second)
	empty, err := s.SnapshotRunRecovery(ctx, altered)
	require.ErrorIs(t, err, execution.ErrRunCallConflict)
	require.Empty(t, empty)
	_, err = s.SnapshotRunRecovery(context.Background(), seal)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = s.SnapshotRunRecovery(cortex.WithScope(ctx, cortex.Scope{Levels: r.Scope.Levels[:1]}), seal)
	require.ErrorIs(t, err, cortex.ErrInvalidScope)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	empty, err = s.SnapshotRunRecovery(cancelled, seal)
	require.ErrorIs(t, err, context.Canceled)
	require.Empty(t, empty)
	// A valid offered seal is insufficient without the matching durable barrier.
	fresh := storedRunCall()
	fresh.Scope = r.Scope
	req := execution.RunCallSealRequest{SchemaVersion: 1, Scope: fresh.Scope, RunID: fresh.RunID, Actor: "recovery", Reason: "read exact evidence"}
	_, digest, err := req.Freeze()
	require.NoError(t, err)
	_, err = s.SnapshotRunRecovery(ctx, execution.RunCallSeal{Request: req, RequestDigest: digest, At: time.Now().UTC()})
	require.ErrorIs(t, err, execution.ErrRunCallConflict)
	emptySeal := snapshotSeal(ctx, t, calls, fresh)
	empty, err = s.SnapshotRunRecovery(ctx, emptySeal)
	require.NoError(t, err)
	require.Empty(t, empty.Calls)
	require.Empty(t, empty.Effects)
	require.NoError(t, empty.Validate())
	t.Run("multiple pages", func(t *testing.T) {
		paged := storedRunCall()
		paged.Scope = r.Scope
		for i := range 129 {
			call := storedRunCall()
			call.Scope = paged.Scope
			call.RunID = paged.RunID
			call.OperationDigest = evidence.Digest([]byte(fmt.Sprintf("call:%d", i)))
			_, writeErr := calls.BeginRunCall(ctx, call)
			require.NoError(t, writeErr)
			effect := storedEffect()
			effect.Scope = paged.Scope
			effect.RunID = paged.RunID
			_, writeErr = effects.BeginEffect(ctx, effect)
			require.NoError(t, writeErr)
		}
		pagedSeal := snapshotSeal(ctx, t, calls, paged)
		result, readErr := s.SnapshotRunRecovery(ctx, pagedSeal)
		require.NoError(t, readErr)
		require.Len(t, result.Calls, 129)
		require.Len(t, result.Effects, 129)
		require.NoError(t, result.Validate())
	})
}

func testRecoverySnapshotFaults(t *testing.T, driver assurance.Driver) {
	transactional, ok := driver.(assurance.RunCallDriver)
	require.True(t, ok)
	ordinary := assurance.New(driver)
	for _, mode := range []string{"rollback", "lost acknowledgment", "cancelled acknowledgment"} {
		t.Run(mode, func(t *testing.T) {
			for _, writer := range []string{"provider", "effect", "local", "snapshot"} {
				t.Run(writer, func(t *testing.T) {
					r := storedBoundEffect()
					ctx := cortex.WithScope(t.Context(), r.Scope)
					_, err := ordinary.BeginRunCall(ctx, *r.RunCall)
					require.NoError(t, err)
					_, err = ordinary.BeginBoundEffect(ctx, r)
					require.NoError(t, err)
					model := snapshotModel(r)
					_, err = ordinary.BeginRunCall(ctx, model)
					require.NoError(t, err)
					seal := snapshotSeal(ctx, t, ordinary, model)
					attempt, cancel := context.WithCancel(ctx)
					defer cancel()
					fault := runCallFaultDriver{Driver: driver, transaction: transactional, rollback: mode == "rollback", loseAck: mode == "lost acknowledgment"}
					if mode == "cancelled acknowledgment" {
						fault.afterCommit = cancel
					}
					faulty := assurance.New(fault)
					proof := snapshotProof(t, model)
					switch writer {
					case "provider":
						err = faulty.PutProviderCallReceipt(attempt, model, proof)
					case "effect":
						err = faulty.CompleteEffect(attempt, r, storedEffectReceipt(t, r))
					case "local":
						err = faulty.CompleteRunCall(attempt, model, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: proof.RequestDigest, ResultDigest: proof.ResultDigest})
					case "snapshot":
						var result execution.RunRecoverySnapshot
						result, err = faulty.SnapshotRunRecovery(attempt, seal)
						require.Empty(t, result)
					}
					require.Error(t, err)
					if mode == "cancelled acknowledgment" {
						require.ErrorIs(t, err, context.Canceled)
					}
					result, err := ordinary.SnapshotRunRecovery(ctx, seal)
					require.NoError(t, err)
					require.NoError(t, result.Validate())
					for _, entry := range result.Calls {
						if entry.Call.Request.ID == model.ID {
							if writer == "provider" {
								require.Equal(t, mode != "rollback", entry.Provider != nil)
							}
							if writer == "local" {
								require.Equal(t, mode != "rollback", entry.Call.Completion != nil)
							}
						}
					}
					if writer == "effect" {
						require.Equal(t, mode != "rollback", result.Effects[0].Receipt != nil)
					}
				})
			}
		})
	}
}

func snapshotModel(r execution.EffectRequest) execution.RunCallRequest {
	model := storedRunCall()
	model.Scope = r.Scope
	model.RunID = r.RunID
	model.OperationDigest = evidence.Digest([]byte("model:1"))
	return model
}
