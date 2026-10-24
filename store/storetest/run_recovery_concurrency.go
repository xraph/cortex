package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store/internal/assurance"
)

type recoveryBarrierDriver struct {
	assurance.Driver
	transaction assurance.RunCallDriver
	entered     chan struct{}
	release     chan struct{}
	once        sync.Once
}

func (d *recoveryBarrierDriver) TransactRunCalls(ctx context.Context, runID id.AgentRunID, callback func(context.Context, assurance.Driver) error) error {
	return d.transaction.TransactRunCalls(ctx, runID, func(tc context.Context, tx assurance.Driver) error {
		if err := callback(tc, tx); err != nil {
			return err
		}
		d.once.Do(func() { close(d.entered) })
		select {
		case <-d.release:
			return nil
		case <-tc.Done():
			return tc.Err()
		}
	})
}

func testRecoverySnapshotSerialization(t *testing.T, driver assurance.Driver) {
	transactional, ok := driver.(assurance.RunCallDriver)
	require.True(t, ok)
	ordinary := assurance.New(driver)
	for _, writer := range []string{"local", "provider", "effect"} {
		t.Run(writer, func(t *testing.T) {
			r := storedBoundEffect()
			ctx, cancel := context.WithTimeout(cortex.WithScope(t.Context(), r.Scope), 15*time.Second)
			defer cancel()
			_, err := ordinary.BeginRunCall(ctx, *r.RunCall)
			require.NoError(t, err)
			_, err = ordinary.BeginBoundEffect(ctx, r)
			require.NoError(t, err)
			model := snapshotModel(r)
			_, err = ordinary.BeginRunCall(ctx, model)
			require.NoError(t, err)
			seal := snapshotSeal(ctx, t, ordinary, model)
			before, err := ordinary.SnapshotRunRecovery(ctx, seal)
			require.NoError(t, err)
			barrier := &recoveryBarrierDriver{Driver: driver, transaction: transactional, entered: make(chan struct{}), release: make(chan struct{})}
			var release sync.Once
			defer release.Do(func() { close(barrier.release) })
			type readResult struct {
				snapshot execution.RunRecoverySnapshot
				err      error
			}
			reads := make(chan readResult, 1)
			go func() {
				snapshot, readErr := assurance.New(barrier).SnapshotRunRecovery(ctx, seal)
				reads <- readResult{snapshot, readErr}
			}()
			select {
			case <-barrier.entered:
			case <-ctx.Done():
				t.Fatal("snapshot did not reach guard")
			}
			writes := make(chan error, 1)
			started := make(chan struct{})
			proof := snapshotProof(t, model)
			receipt := storedEffectReceipt(t, r)
			go func() {
				close(started)
				switch writer {
				case "local":
					writes <- ordinary.CompleteRunCall(ctx, model, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: proof.RequestDigest, ResultDigest: proof.ResultDigest})
				case "provider":
					writes <- ordinary.PutProviderCallReceipt(ctx, model, proof)
				case "effect":
					writes <- ordinary.CompleteEffect(ctx, r, receipt)
				}
			}()
			<-started
			select {
			case writeErr := <-writes:
				t.Fatalf("late %s writer escaped snapshot guard: %v", writer, writeErr)
			case <-time.After(150 * time.Millisecond):
			}
			release.Do(func() { close(barrier.release) })
			select {
			case read := <-reads:
				require.NoError(t, read.err)
				require.Equal(t, before.Digest, read.snapshot.Digest)
			case <-ctx.Done():
				t.Fatal("snapshot failed to finish")
			}
			select {
			case writeErr := <-writes:
				require.NoError(t, writeErr)
			case <-ctx.Done():
				t.Fatal("late write failed to finish")
			}
			after, err := ordinary.SnapshotRunRecovery(ctx, seal)
			require.NoError(t, err)
			require.NotEqual(t, before.Digest, after.Digest)
		})
	}
}
