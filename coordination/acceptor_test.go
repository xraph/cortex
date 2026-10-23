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

type taskAcceptanceFenceFunc func(context.Context, coordination.TaskAcceptanceRequest, func(coordination.TaskAcceptanceRequest) error) error

func (f taskAcceptanceFenceFunc) WithTaskAcceptanceFence(ctx context.Context, req coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
	return f(ctx, req, next)
}

type taskAcceptanceAuthorizeFunc func(context.Context, coordination.TaskAcceptanceRequest) error

func (f taskAcceptanceAuthorizeFunc) AuthorizeTaskAcceptance(ctx context.Context, req coordination.TaskAcceptanceRequest) error {
	return f(ctx, req)
}

type taskAcceptanceStorage struct {
	coordination.TaskAcceptanceStore
	accept func(context.Context, coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error)
}

func (s taskAcceptanceStorage) AcceptTaskResult(ctx context.Context, req coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
	return s.accept(ctx, req)
}

func TestTaskAcceptorHoldsAuthorityFenceThroughStorage(t *testing.T) {
	receipt := taskAcceptanceFixture(t)
	receipt.Fresh = true
	ctx := cortex.WithScope(context.Background(), receipt.Request.Current.Scope)
	var steps []string
	held := false
	fence := taskAcceptanceFenceFunc(func(_ context.Context, current coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
		held = true
		steps = append(steps, "acquire")
		defer func() { held = false; steps = append(steps, "release") }()
		return next(current)
	})
	authorizer := taskAcceptanceAuthorizeFunc(func(_ context.Context, _ coordination.TaskAcceptanceRequest) error {
		require.True(t, held)
		steps = append(steps, "authorize")
		return nil
	})
	storage := taskAcceptanceStorage{accept: func(_ context.Context, req coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
		require.True(t, held)
		require.Equal(t, receipt.Request, req)
		steps = append(steps, "store")
		return receipt, nil
	}}
	acceptor, err := coordination.NewTaskAcceptor(storage, fence, authorizer)
	require.NoError(t, err)
	got, err := acceptor.Accept(ctx, receipt.Request)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	require.Equal(t, []string{"acquire", "authorize", "store", "release"}, steps)
}

func TestTaskAcceptorDenialAndChangedContextNeverWrite(t *testing.T) {
	denied := errors.New("revoked")
	for _, mode := range []string{"denied", "changed_context", "changed_actor", "scope", "cancelled", "missing_callback", "duplicate_callback", "lost_ack"} {
		t.Run(mode, func(t *testing.T) {
			receipt := taskAcceptanceFixture(t)
			ctx := cortex.WithScope(context.Background(), receipt.Request.Current.Scope)
			if mode == "scope" {
				ctx = context.Background()
			}
			if mode == "cancelled" {
				bounded, cancel := context.WithCancel(ctx)
				cancel()
				ctx = bounded
			}
			calls := 0
			storage := taskAcceptanceStorage{accept: func(context.Context, coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
				calls++
				return receipt, nil
			}}
			fence := taskAcceptanceFenceFunc(func(_ context.Context, current coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
				switch mode {
				case "changed_context":
					current.Current.Context.AuthorityRevision = "revoked"
				case "changed_actor":
					current.Actor = "another host"
				case "missing_callback":
					return nil
				case "duplicate_callback":
					_ = next(current)
					_ = next(current)
					return nil
				case "lost_ack":
					_ = next(current)
					return denied
				}
				return next(current)
			})
			authorizer := taskAcceptanceAuthorizeFunc(func(context.Context, coordination.TaskAcceptanceRequest) error {
				if mode == "denied" {
					return denied
				}
				return nil
			})
			acceptor, err := coordination.NewTaskAcceptor(storage, fence, authorizer)
			require.NoError(t, err)
			result, err := acceptor.Accept(ctx, receipt.Request)
			require.Error(t, err)
			require.Zero(t, result)
			if mode == "duplicate_callback" || mode == "lost_ack" {
				require.Equal(t, 1, calls)
			} else {
				require.Zero(t, calls)
			}
		})
	}
}

func TestTaskAcceptorDetachesHostAndStorageValues(t *testing.T) {
	receipt := taskAcceptanceFixture(t)
	ctx := cortex.WithScope(context.Background(), receipt.Request.Current.Scope)
	fence := taskAcceptanceFenceFunc(func(_ context.Context, current coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
		return next(current)
	})
	authorizer := taskAcceptanceAuthorizeFunc(func(_ context.Context, current coordination.TaskAcceptanceRequest) error {
		current.Current.Scope.Levels[0].Value = "mutated"
		return nil
	})
	storage := taskAcceptanceStorage{accept: func(_ context.Context, req coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
		require.Equal(t, receipt.Request, req)
		return receipt, nil
	}}
	acceptor, err := coordination.NewTaskAcceptor(storage, fence, authorizer)
	require.NoError(t, err)
	got, err := acceptor.Accept(ctx, receipt.Request)
	require.NoError(t, err)
	receipt.Request.Current.Scope.Levels[0].Value = "storage mutation"
	require.NoError(t, got.Validate())
}

func TestTaskAcceptorEscapedCallbackCannotWrite(t *testing.T) {
	receipt := taskAcceptanceFixture(t)
	ctx := cortex.WithScope(context.Background(), receipt.Request.Current.Scope)
	var escaped func(coordination.TaskAcceptanceRequest) error
	calls := 0
	storage := taskAcceptanceStorage{accept: func(context.Context, coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
		calls++
		return receipt, nil
	}}
	fence := taskAcceptanceFenceFunc(func(_ context.Context, _ coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
		escaped = next
		return nil
	})
	authorizer := taskAcceptanceAuthorizeFunc(func(context.Context, coordination.TaskAcceptanceRequest) error { return nil })
	acceptor, err := coordination.NewTaskAcceptor(storage, fence, authorizer)
	require.NoError(t, err)
	_, err = acceptor.Accept(ctx, receipt.Request)
	require.Error(t, err)
	require.Error(t, escaped(receipt.Request))
	require.Zero(t, calls)
}

func TestTaskAcceptorCancelsUnfinishedHostCallback(t *testing.T) {
	receipt := taskAcceptanceFixture(t)
	ctx := cortex.WithScope(context.Background(), receipt.Request.Current.Scope)
	started := make(chan struct{})
	finished := make(chan error, 1)
	storage := taskAcceptanceStorage{accept: func(context.Context, coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
		t.Error("cancelled authorization reached storage")
		return receipt, nil
	}}
	fence := taskAcceptanceFenceFunc(func(_ context.Context, current coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
		go func() { finished <- next(current) }()
		<-started
		return nil
	})
	authorizer := taskAcceptanceAuthorizeFunc(func(ctx context.Context, _ coordination.TaskAcceptanceRequest) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	})
	acceptor, err := coordination.NewTaskAcceptor(storage, fence, authorizer)
	require.NoError(t, err)
	got, err := acceptor.Accept(ctx, receipt.Request)
	require.Error(t, err)
	require.Zero(t, got)
	require.Error(t, <-finished)
}

func TestTaskAcceptorRejectsMissingPortsAndBadReceipts(t *testing.T) {
	receipt := taskAcceptanceFixture(t)
	ctx := cortex.WithScope(context.Background(), receipt.Request.Current.Scope)
	fence := taskAcceptanceFenceFunc(func(_ context.Context, current coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
		return next(current)
	})
	authorizer := taskAcceptanceAuthorizeFunc(func(context.Context, coordination.TaskAcceptanceRequest) error { return nil })
	storage := taskAcceptanceStorage{accept: func(context.Context, coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
		return receipt, nil
	}}
	for _, mode := range []string{"storage", "fence", "authorizer"} {
		var s coordination.TaskAcceptanceStore = storage
		var f coordination.TaskAcceptanceFence = fence
		var a coordination.TaskAcceptanceAuthorizer = authorizer
		switch mode {
		case "storage":
			s = (*taskAcceptanceStorage)(nil)
		case "fence":
			f = taskAcceptanceFenceFunc(nil)
		case "authorizer":
			a = taskAcceptanceAuthorizeFunc(nil)
		}
		_, err := coordination.NewTaskAcceptor(s, f, a)
		require.Error(t, err)
	}
	for _, changedRequest := range []bool{false, true} {
		wrong := taskAcceptanceFixture(t)
		if !changedRequest {
			wrong = receipt
			wrong.Outbox.Type = "unverified"
		}
		storage.accept = func(context.Context, coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
			return wrong, nil
		}
		acceptor, err := coordination.NewTaskAcceptor(storage, fence, authorizer)
		require.NoError(t, err)
		got, err := acceptor.Accept(ctx, receipt.Request)
		require.Error(t, err)
		require.Zero(t, got)
	}
}

func TestTaskAcceptorChecksDependenciesAndReauthorizesHistoricalRetry(t *testing.T) {
	receipt := taskAcceptanceFixture(t)
	receipt.Request.Dependencies = []coordination.AcceptedDependency{{AcceptanceID: id.New(coordination.PrefixTaskAcceptance), Digest: receipt.Digest, Current: receipt.Request.Current}}
	var err error
	receipt.Request, receipt.RequestDigest, err = receipt.Request.Freeze()
	require.NoError(t, err)
	receipt, err = coordination.SealTaskAcceptance(receipt)
	require.NoError(t, err)
	ctx := cortex.WithScope(context.Background(), receipt.Request.Current.Scope)
	allowed, stale := true, false
	writes, authorizations := 0, 0
	fence := taskAcceptanceFenceFunc(func(_ context.Context, current coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
		if stale {
			current.Dependencies[0].Current.Context.PolicyRevision = "changed"
		}
		return next(current)
	})
	authorizer := taskAcceptanceAuthorizeFunc(func(_ context.Context, req coordination.TaskAcceptanceRequest) error {
		authorizations++
		require.Equal(t, receipt.Request.Dependencies, req.Dependencies)
		if !allowed {
			return errors.New("revoked")
		}
		return nil
	})
	storage := taskAcceptanceStorage{accept: func(context.Context, coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
		writes++
		return receipt, nil
	}}
	acceptor, err := coordination.NewTaskAcceptor(storage, fence, authorizer)
	require.NoError(t, err)
	got, err := acceptor.Accept(ctx, receipt.Request)
	require.NoError(t, err)
	require.False(t, got.Fresh)
	allowed = false
	_, err = acceptor.Accept(ctx, receipt.Request)
	require.Error(t, err)
	require.Equal(t, 2, authorizations)
	require.Equal(t, 1, writes)
	allowed, stale = true, true
	_, err = acceptor.Accept(ctx, receipt.Request)
	require.ErrorIs(t, err, coordination.ErrTaskAcceptanceConflict)
	require.Equal(t, 1, writes)
	require.Equal(t, 2, authorizations)
}
