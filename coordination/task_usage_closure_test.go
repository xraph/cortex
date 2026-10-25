package coordination_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
)

func usageClosureFixture(t *testing.T) (context.Context, coordination.TaskUsageClosureRequest, coordination.TaskUsageClosure) {
	t.Helper()
	s := taskUsageFixture(t)
	r := coordination.TaskUsageClosureRequest{SchemaVersion: 1, Usage: coordination.TaskUsageRequest{Claim: s.Claim.Token(), Seal: s.Journal.Seal, SnapshotDigest: s.Journal.Digest}, UsageDigest: s.Digest, Actor: "operator", Reason: "finish task accounting"}
	r, digest, err := r.Freeze()
	require.NoError(t, err)
	closed, err := coordination.SealTaskUsageClosure(coordination.TaskUsageClosure{SchemaVersion: 1, Request: r, RequestDigest: digest, At: s.Claim.At})
	require.NoError(t, err)
	return cortex.WithScope(context.Background(), s.Claim.Scope), r, closed
}

type usageClosureFence func(context.Context, coordination.TaskUsageClosureRequest, func(coordination.TaskUsageClosureRequest) error) error

func (f usageClosureFence) WithTaskUsageClosureFence(ctx context.Context, r coordination.TaskUsageClosureRequest, next func(coordination.TaskUsageClosureRequest) error) error {
	return f(ctx, r, next)
}

type usageClosureAuthorizer func(context.Context, coordination.TaskUsageClosureRequest) error

func (f usageClosureAuthorizer) AuthorizeTaskUsageClosure(ctx context.Context, r coordination.TaskUsageClosureRequest) error {
	return f(ctx, r)
}

type usageClosureStore struct {
	coordination.TaskUsageClosureStore
	close func(context.Context, coordination.TaskUsageClosureRequest) (coordination.TaskUsageClosure, error)
}

func (s usageClosureStore) CloseTaskUsage(ctx context.Context, r coordination.TaskUsageClosureRequest) (coordination.TaskUsageClosure, error) {
	return s.close(ctx, r)
}

func TestTaskUsageClosureRequiresCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"allowed", "denied", "changed request", "changed receipt", "cancelled", "missing callback", "duplicate callback"} {
		t.Run(mode, func(t *testing.T) {
			ctx, r, receipt := usageClosureFixture(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			var events []string
			store := usageClosureStore{close: func(_ context.Context, got coordination.TaskUsageClosureRequest) (coordination.TaskUsageClosure, error) {
				events = append(events, "store")
				require.Equal(t, r, got)
				if mode == "changed receipt" {
					receipt.Digest = evidence.Digest([]byte("wrong"))
				}
				return receipt, nil
			}}
			fence := usageClosureFence(func(_ context.Context, got coordination.TaskUsageClosureRequest, next func(coordination.TaskUsageClosureRequest) error) error {
				events = append(events, "fence")
				if mode == "missing callback" {
					return nil
				}
				if mode == "changed request" {
					got.Actor = "other"
				}
				if err := next(got); err != nil {
					return err
				}
				if mode == "duplicate callback" {
					return next(got)
				}
				return nil
			})
			authorize := usageClosureAuthorizer(func(_ context.Context, got coordination.TaskUsageClosureRequest) error {
				events = append(events, "authorize")
				got.Usage.Seal.Request.Scope.Levels[0].Value = "mutated-port-copy"
				if mode == "denied" {
					return errors.New("denied")
				}
				if mode == "cancelled" {
					cancel()
				}
				return nil
			})
			closer, err := coordination.NewTaskUsageCloser(store, fence, authorize)
			require.NoError(t, err)
			got, err := closer.Close(ctx, r)
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

func TestTaskUsageClosureRejectsMissingPortsAndInvalidBinding(t *testing.T) {
	_, r, receipt := usageClosureFixture(t)
	_, err := coordination.NewTaskUsageCloser(nil, nil, nil)
	require.Error(t, err)
	r.UsageDigest = ""
	_, _, err = r.Freeze()
	require.Error(t, err)
	receipt.At = receipt.At.Add(-1)
	require.Error(t, receipt.Validate())
}
