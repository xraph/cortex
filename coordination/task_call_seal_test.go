package coordination_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
)

type taskCallSealFence func(context.Context, coordination.TaskCallSealRequest, func(coordination.TaskCallSealRequest) error) error

func (f taskCallSealFence) WithTaskCallSealFence(ctx context.Context, r coordination.TaskCallSealRequest, next func(coordination.TaskCallSealRequest) error) error {
	return f(ctx, r, next)
}

type taskCallSealAuthorizer func(context.Context, coordination.TaskCallSealRequest) error

func (f taskCallSealAuthorizer) AuthorizeTaskCallSeal(ctx context.Context, r coordination.TaskCallSealRequest) error {
	return f(ctx, r)
}

type taskCallSealStore struct {
	close func(context.Context, coordination.TaskCallSealRequest) (execution.RunCallSeal, error)
}

func (s taskCallSealStore) SealTaskCalls(ctx context.Context, r coordination.TaskCallSealRequest) (execution.RunCallSeal, error) {
	return s.close(ctx, r)
}

func TestTaskCallSealRequiresCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"allowed", "denied", "changed request", "changed receipt", "cancelled", "missing callback", "duplicate callback"} {
		t.Run(mode, func(t *testing.T) {
			ctx, fixture, _ := settlementServiceFixture(t)
			r := coordination.TaskCallSealRequest{SchemaVersion: 1, Claim: fixture.claim.Token(), ClaimDigest: fixture.claim.Digest, Seal: fixture.seal.Request}
			receipt := *fixture.seal
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			var events []string
			store := taskCallSealStore{close: func(_ context.Context, got coordination.TaskCallSealRequest) (execution.RunCallSeal, error) {
				events = append(events, "store")
				require.Equal(t, r, got)
				if mode == "changed receipt" {
					receipt.RequestDigest = evidence.Digest([]byte("wrong"))
				}
				return receipt, nil
			}}
			fence := taskCallSealFence(func(_ context.Context, got coordination.TaskCallSealRequest, next func(coordination.TaskCallSealRequest) error) error {
				events = append(events, "fence")
				if mode == "missing callback" {
					return nil
				}
				if mode == "changed request" {
					got.Seal.Actor = "other"
				}
				if err := next(got); err != nil {
					return err
				}
				if mode == "duplicate callback" {
					return next(got)
				}
				return nil
			})
			authorize := taskCallSealAuthorizer(func(_ context.Context, got coordination.TaskCallSealRequest) error {
				events = append(events, "authorize")
				got.Seal.Scope.Levels[0].Value = "mutated-port-copy"
				if mode == "denied" {
					return errors.New("denied")
				}
				if mode == "cancelled" {
					cancel()
				}
				return nil
			})
			closer, err := coordination.NewTaskCallSealer(store, fence, authorize)
			require.NoError(t, err)
			got, err := closer.Seal(ctx, r)
			if mode == "allowed" {
				require.NoError(t, err)
				require.Equal(t, receipt, got)
				require.Equal(t, []string{"fence", "authorize", "store"}, events)
			} else {
				require.Error(t, err)
				require.Empty(t, got)
				if mode != "changed receipt" && mode != "duplicate callback" {
					require.NotContains(t, events, "store")
				}
				if mode == "duplicate callback" {
					require.Equal(t, []string{"fence", "authorize", "store"}, events)
				}
			}
		})
	}
}
