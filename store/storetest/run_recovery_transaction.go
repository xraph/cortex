package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

// RunRecoveryTransactions checks evidence revalidation inside the real plan
// transaction. These storage fixtures do not authenticate proof or adopt tasks.
func RunRecoveryTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	repo := assurance.New(driver)
	fixture := func(t *testing.T) (context.Context, id.ID, execution.EffectRequest, execution.RunRecoverySnapshot) {
		t.Helper()
		ctx, claim := claimFixture(t, backend)
		scope, err := cortex.RequireScope(ctx)
		require.NoError(t, err)
		r := storedBoundEffect()
		r.Scope = scope
		r.RunCall.Scope = scope
		_, err = repo.BeginRunCall(ctx, *r.RunCall)
		require.NoError(t, err)
		_, err = repo.BeginBoundEffect(ctx, r)
		require.NoError(t, err)
		model := snapshotModel(r)
		_, err = repo.BeginRunCall(ctx, model)
		require.NoError(t, err)
		seal := snapshotSeal(ctx, t, repo, model)
		snapshot, err := repo.SnapshotRunRecovery(ctx, seal)
		require.NoError(t, err)
		return ctx, claim.PlanID, r, snapshot
	}
	t.Run("transaction required", func(t *testing.T) {
		ctx, planID, _, snapshot := fixture(t)
		got, err := repo.LockRunRecoverySnapshot(ctx, snapshot.Seal, snapshot.Digest)
		require.ErrorIs(t, err, execution.ErrRunCallUnavailable)
		require.Empty(t, got)
		var escaped assurance.Driver
		require.NoError(t, plans.TransactPlan(ctx, planID, func(_ context.Context, tx assurance.Driver) error { escaped = tx; return nil }))
		got, err = assurance.New(escaped).LockRunRecoverySnapshot(ctx, snapshot.Seal, snapshot.Digest)
		require.Error(t, err)
		require.Empty(t, got)
		require.NoError(t, plans.TransactPlan(ctx, planID, func(tc context.Context, tx assurance.Driver) error {
			got, readErr := assurance.New(struct{ assurance.Driver }{tx}).LockRunRecoverySnapshot(tc, snapshot.Seal, snapshot.Digest)
			require.ErrorIs(t, readErr, execution.ErrRunCallUnavailable)
			require.Empty(t, got)
			return nil
		}))
	})
	t.Run("scope seal and digest", func(t *testing.T) {
		ctx, planID, _, snapshot := fixture(t)
		for _, mode := range []string{"scope", "missing scope", "seal", "digest", "invalid digest", "cancelled"} {
			t.Run(mode, func(t *testing.T) {
				require.NoError(t, plans.TransactPlan(ctx, planID, func(tc context.Context, tx assurance.Driver) error {
					seal, digest := snapshot.Seal, snapshot.Digest
					switch mode {
					case "scope":
						tc = cortex.WithScope(tc, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
					case "missing scope":
						tc = context.Background()
					case "seal":
						seal.At = seal.At.Add(time.Second)
					case "digest":
						digest = evidence.Digest([]byte("different"))
					case "invalid digest":
						digest = "not a digest"
					case "cancelled":
						var cancel context.CancelFunc
						tc, cancel = context.WithCancel(tc)
						cancel()
					}
					got, readErr := assurance.New(tx).LockRunRecoverySnapshot(tc, seal, digest)
					require.Error(t, readErr)
					require.Empty(t, got)
					return nil
				}))
			})
		}
	})
	t.Run("changed evidence aborts plan write", func(t *testing.T) {
		ctx, planID, r, snapshot := fixture(t)
		require.NoError(t, repo.CompleteEffect(ctx, r, storedEffectReceipt(t, r)))
		marker := recoveryTransactionMarker(snapshot.Seal.Request.Scope)
		err := plans.TransactPlan(ctx, planID, func(tc context.Context, tx assurance.Driver) error {
			if writeErr := tx.Insert(tc, marker); writeErr != nil {
				return writeErr
			}
			got, readErr := assurance.New(tx).LockRunRecoverySnapshot(tc, snapshot.Seal, snapshot.Digest)
			require.Empty(t, got)
			return readErr
		})
		require.ErrorIs(t, err, execution.ErrRunCallConflict)
		rows, err := driver.Select(ctx, assurance.Query{Kind: marker.Kind, ID: marker.ID, Limit: 1})
		require.NoError(t, err)
		require.Empty(t, rows)
	})
	t.Run("rollback releases both locks", func(t *testing.T) {
		ctx, planID, r, snapshot := fixture(t)
		marker := recoveryTransactionMarker(snapshot.Seal.Request.Scope)
		injected := errors.New("rollback after recovery evidence")
		err := plans.TransactPlan(ctx, planID, func(tc context.Context, tx assurance.Driver) error {
			got, readErr := assurance.New(tx).LockRunRecoverySnapshot(tc, snapshot.Seal, snapshot.Digest)
			if readErr != nil {
				return readErr
			}
			require.Equal(t, snapshot.Digest, got.Digest)
			if writeErr := tx.Insert(tc, marker); writeErr != nil {
				return writeErr
			}
			return injected
		})
		require.ErrorIs(t, err, injected)
		rows, err := driver.Select(ctx, assurance.Query{Kind: marker.Kind, ID: marker.ID, Limit: 1})
		require.NoError(t, err)
		require.Empty(t, rows)
		require.NoError(t, repo.CompleteEffect(ctx, r, storedEffectReceipt(t, r)))
		require.NoError(t, plans.TransactPlan(ctx, planID, func(tc context.Context, tx assurance.Driver) error { return tx.Insert(tc, marker) }))
	})
	t.Run("cancelled plan transaction", func(t *testing.T) {
		base, planID, r, snapshot := fixture(t)
		ctx, cancel := context.WithCancel(base)
		defer cancel()
		marker := recoveryTransactionMarker(r.Scope)
		err := plans.TransactPlan(ctx, planID, func(tc context.Context, tx assurance.Driver) error {
			_, readErr := assurance.New(tx).LockRunRecoverySnapshot(tc, snapshot.Seal, snapshot.Digest)
			if readErr != nil {
				return readErr
			}
			if writeErr := tx.Insert(tc, marker); writeErr != nil {
				return writeErr
			}
			cancel()
			return tc.Err()
		})
		require.ErrorIs(t, err, context.Canceled)
		rows, err := driver.Select(base, assurance.Query{Kind: marker.Kind, ID: marker.ID, Limit: 1})
		require.NoError(t, err)
		require.Empty(t, rows)
		require.NoError(t, repo.CompleteEffect(base, r, storedEffectReceipt(t, r)))
	})
	for _, writer := range []string{"local", "provider", "effect"} {
		t.Run("holds lock through plan commit "+writer, func(t *testing.T) {
			base, planID, r, snapshot := fixture(t)
			ctx, cancel := context.WithTimeout(base, 15*time.Second)
			defer cancel()
			marker := recoveryTransactionMarker(r.Scope)
			entered, release := make(chan struct{}), make(chan struct{})
			var enteredOnce, releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			commit := make(chan error, 1)
			go func() {
				commit <- plans.TransactPlan(ctx, planID, func(tc context.Context, tx assurance.Driver) error {
					got, readErr := assurance.New(tx).LockRunRecoverySnapshot(tc, snapshot.Seal, snapshot.Digest)
					if readErr != nil {
						return readErr
					}
					if got.Digest != snapshot.Digest {
						return execution.ErrRunCallConflict
					}
					enteredOnce.Do(func() { close(entered) })
					select {
					case <-release:
					case <-tc.Done():
						return tc.Err()
					}
					return tx.Insert(tc, marker)
				})
			}()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("transaction did not acquire recovery locks")
			}
			model := snapshotModel(r)
			// Use the retained model request, not a fresh attempt in its occupied slot.
			for _, entry := range snapshot.Calls {
				if entry.Call.Request.Kind == execution.RunCallComplete {
					model = entry.Call.Request
				}
			}
			proof := snapshotProof(t, model)
			receipt := storedEffectReceipt(t, r)
			late := make(chan error, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				switch writer {
				case "local":
					late <- repo.CompleteRunCall(ctx, model, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: proof.RequestDigest, ResultDigest: proof.ResultDigest})
				case "provider":
					late <- repo.PutProviderCallReceipt(ctx, model, proof)
				case "effect":
					late <- repo.CompleteEffect(ctx, r, receipt)
				}
			}()
			<-started
			select {
			case writeErr := <-late:
				t.Fatalf("receipt escaped plan transaction: %v", writeErr)
			case <-time.After(150 * time.Millisecond):
			}
			releaseOnce.Do(func() { close(release) })
			select {
			case commitErr := <-commit:
				require.NoError(t, commitErr)
			case <-ctx.Done():
				t.Fatal("commit did not finish")
			}
			select {
			case writeErr := <-late:
				require.NoError(t, writeErr)
			case <-ctx.Done():
				t.Fatal("receipt did not finish")
			}
			rows, err := driver.Select(ctx, assurance.Query{Kind: marker.Kind, ID: marker.ID, Limit: 1})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			after, err := repo.SnapshotRunRecovery(ctx, snapshot.Seal)
			require.NoError(t, err)
			require.NotEqual(t, snapshot.Digest, after.Digest)
		})
	}
}

func recoveryTransactionMarker(scope cortex.Scope) assurance.Record {
	columns := assurance.Columns(scope)
	return assurance.Record{Kind: "recovery_transaction_fixture", ID: id.NewAgentRunID().String(), ScopeCanon: scope.Canonical(), ScopeL0: columns[0], ScopeL1: columns[1], ScopeL2: columns[2], ScopeExtra: "{}", Body: "{}", Digest: evidence.Digest([]byte("{}"))}
}
