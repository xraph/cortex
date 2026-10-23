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

type taskDispatchFenceFunc func(context.Context, coordination.TaskDispatchRequest, func(coordination.TaskDispatchRequest) error) error

func (f taskDispatchFenceFunc) WithTaskDispatchFence(ctx context.Context, req coordination.TaskDispatchRequest, next func(coordination.TaskDispatchRequest) error) error {
	return f(ctx, req, next)
}

type taskDispatchAuthorizeFunc func(context.Context, coordination.TaskDispatchRequest) error

func (f taskDispatchAuthorizeFunc) AuthorizeTaskDispatch(ctx context.Context, req coordination.TaskDispatchRequest) error {
	return f(ctx, req)
}

type taskDispatchStorage struct {
	coordination.TaskDispatchStore
	dispatch func(context.Context, coordination.TaskDispatchRequest) (coordination.TaskDispatch, error)
}

func (s taskDispatchStorage) DispatchReadyTask(ctx context.Context, req coordination.TaskDispatchRequest) (coordination.TaskDispatch, error) {
	return s.dispatch(ctx, req)
}

func TestTaskDispatchAdmitterHoldsFenceAndDetachesValues(t *testing.T) {
	req, receipt := taskDispatchFixture(t)
	receipt.Fresh = true
	ctx := cortex.WithScope(context.Background(), req.Scope)
	var steps []string
	held := false
	fence := taskDispatchFenceFunc(func(_ context.Context, current coordination.TaskDispatchRequest, next func(coordination.TaskDispatchRequest) error) error {
		held = true
		steps = append(steps, "acquire")
		defer func() { held = false; steps = append(steps, "release") }()
		return next(current)
	})
	authorizer := taskDispatchAuthorizeFunc(func(_ context.Context, current coordination.TaskDispatchRequest) error {
		require.True(t, held)
		require.Equal(t, req, current)
		steps = append(steps, "authorize")
		current.Scope.Levels[0].Value = "mutated"
		return nil
	})
	storage := taskDispatchStorage{dispatch: func(_ context.Context, current coordination.TaskDispatchRequest) (coordination.TaskDispatch, error) {
		require.True(t, held)
		require.Equal(t, req, current)
		steps = append(steps, "store")
		return receipt, nil
	}}
	admitter, err := coordination.NewTaskDispatchAdmitter(storage, fence, authorizer)
	require.NoError(t, err)
	got, err := admitter.Admit(ctx, req)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	require.Equal(t, []string{"acquire", "authorize", "store", "release"}, steps)
	receipt.Assignment.Handoff.Criteria[0] = "mutated"
	require.NoError(t, got.Validate(req))
}

func TestTaskDispatchAdmitterRejectsChangedAuthorityAndInvalidReceipts(t *testing.T) {
	for _, mode := range []string{"denied", "changed_run", "changed_parent", "changed_assignment", "wrong_scope", "cancelled", "cancel_in_authorizer", "missing_callback", "duplicate_callback", "lost_ack", "bad_receipt", "unrelated_receipt", "forged_digest"} {
		t.Run(mode, func(t *testing.T) {
			req, receipt := taskDispatchFixture(t)
			ctx := cortex.WithScope(context.Background(), req.Scope)
			if mode == "wrong_scope" {
				ctx = context.Background()
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			calls := 0
			storage := taskDispatchStorage{dispatch: func(context.Context, coordination.TaskDispatchRequest) (coordination.TaskDispatch, error) {
				calls++
				if mode == "bad_receipt" {
					receipt.Outbox.Type = "completed"
				}
				if mode == "unrelated_receipt" || mode == "forged_digest" {
					expected := receipt.RequestDigest
					_, receipt = taskDispatchFixture(t)
					if mode == "forged_digest" {
						receipt.RequestDigest = expected
					}
				}
				return receipt, nil
			}}
			fence := taskDispatchFenceFunc(func(_ context.Context, current coordination.TaskDispatchRequest, next func(coordination.TaskDispatchRequest) error) error {
				switch mode {
				case "changed_run":
					current.RunID = id.NewAgentRunID()
				case "changed_parent":
					current.ParentRunRevision++
				case "changed_assignment":
					current.AssignmentRevision++
				case "missing_callback":
					return nil
				case "duplicate_callback":
					_ = next(current)
					_ = next(current)
					return nil
				case "lost_ack":
					_ = next(current)
					return errors.New("lost acknowledgment")
				}
				return next(current)
			})
			authorizer := taskDispatchAuthorizeFunc(func(context.Context, coordination.TaskDispatchRequest) error {
				if mode == "denied" {
					return errors.New("revoked")
				}
				if mode == "cancel_in_authorizer" {
					cancel()
				}
				return nil
			})
			admitter, err := coordination.NewTaskDispatchAdmitter(storage, fence, authorizer)
			require.NoError(t, err)
			got, err := admitter.Admit(ctx, req)
			require.Error(t, err)
			require.Zero(t, got)
			expected := 0
			if mode == "duplicate_callback" || mode == "lost_ack" || mode == "bad_receipt" || mode == "unrelated_receipt" || mode == "forged_digest" {
				expected = 1
			}
			require.Equal(t, expected, calls)
		})
	}
}

func TestTaskDispatchAdmitterChecksChainAndReauthorizesHistory(t *testing.T) {
	req, receipt := taskDispatchFixture(t)
	accepted := taskAcceptanceFixture(t)
	accepted.Request.Current.Scope = req.Scope
	req.Dependencies = []coordination.AcceptedDependency{{AcceptanceID: accepted.Request.ID, Digest: accepted.Digest, Current: accepted.Request.Current}}
	var err error
	req, receipt.RequestDigest, err = req.Freeze()
	require.NoError(t, err)
	allowed, stale := true, false
	calls, authorizations := 0, 0
	fence := taskDispatchFenceFunc(func(_ context.Context, current coordination.TaskDispatchRequest, next func(coordination.TaskDispatchRequest) error) error {
		if stale {
			current.Dependencies[0].Current.Context.AuthorityRevision = "revoked"
		}
		return next(current)
	})
	authorizer := taskDispatchAuthorizeFunc(func(_ context.Context, current coordination.TaskDispatchRequest) error {
		authorizations++
		require.Equal(t, req.Dependencies, current.Dependencies)
		if !allowed {
			return errors.New("revoked")
		}
		return nil
	})
	storage := taskDispatchStorage{dispatch: func(context.Context, coordination.TaskDispatchRequest) (coordination.TaskDispatch, error) {
		calls++
		return receipt, nil
	}}
	admitter, err := coordination.NewTaskDispatchAdmitter(storage, fence, authorizer)
	require.NoError(t, err)
	ctx := cortex.WithScope(context.Background(), req.Scope)
	got, err := admitter.Admit(ctx, req)
	require.NoError(t, err)
	require.False(t, got.Fresh)
	allowed = false
	_, err = admitter.Admit(ctx, req)
	require.Error(t, err)
	require.Equal(t, 2, authorizations)
	require.Equal(t, 1, calls)
	allowed, stale = true, true
	_, err = admitter.Admit(ctx, req)
	require.ErrorIs(t, err, coordination.ErrClaimConflict)
	require.Equal(t, 2, authorizations)
	require.Equal(t, 1, calls)
}

func TestTaskDispatchAdmitterRequiresPorts(t *testing.T) {
	fence := taskDispatchFenceFunc(func(_ context.Context, current coordination.TaskDispatchRequest, next func(coordination.TaskDispatchRequest) error) error {
		return next(current)
	})
	authorizer := taskDispatchAuthorizeFunc(func(context.Context, coordination.TaskDispatchRequest) error { return nil })
	for _, mode := range []string{"storage", "fence", "authorizer"} {
		var s coordination.TaskDispatchStore = taskDispatchStorage{}
		var f coordination.TaskDispatchFence = fence
		var a coordination.TaskDispatchAuthorizer = authorizer
		switch mode {
		case "storage":
			s = (*taskDispatchStorage)(nil)
		case "fence":
			f = taskDispatchFenceFunc(nil)
		case "authorizer":
			a = taskDispatchAuthorizeFunc(nil)
		}
		_, err := coordination.NewTaskDispatchAdmitter(s, f, a)
		require.Error(t, err)
	}
}
