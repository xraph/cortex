package coordination_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
)

type taskRecoveryFenceFunc func(context.Context, coordination.TaskAcceptanceRequest, func(coordination.TaskAcceptanceRequest) error) error

func (f taskRecoveryFenceFunc) WithTaskRecoveryFence(ctx context.Context, r coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
	return f(ctx, r, next)
}

type taskRecoveryAuthorizeFunc func(context.Context, coordination.TaskAcceptanceRequest) error

func (f taskRecoveryAuthorizeFunc) AuthorizeTaskRecovery(ctx context.Context, r coordination.TaskAcceptanceRequest) error {
	return f(ctx, r)
}

type taskRecoveryStorageFunc func(context.Context, coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error)

func (f taskRecoveryStorageFunc) AdoptTaskResult(ctx context.Context, r coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
	return f(ctx, r)
}

func TestTaskAdopterFencesAndAuthenticatesBeforeStorage(t *testing.T) {
	receipt := taskAcceptanceFixture(t, true)
	receipt.Fresh = true
	ctx := cortex.WithScope(t.Context(), receipt.Request.Current.Scope)
	held := false
	var steps []string
	fence := taskRecoveryFenceFunc(func(_ context.Context, r coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
		held = true
		steps = append(steps, "fence")
		defer func() { held = false; steps = append(steps, "release") }()
		return next(r)
	})
	auth := taskRecoveryAuthorizeFunc(func(bounded context.Context, r coordination.TaskAcceptanceRequest) error {
		require.True(t, held)
		deadline, ok := bounded.Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), 30*time.Second)
		steps = append(steps, "authenticate proof and authorize")
		r.Recovery.ProofRef = "caller mutation"
		r.Recovery.Seal.Request.Scope.Levels[0].Value = "mutated"
		return nil
	})
	storage := taskRecoveryStorageFunc(func(_ context.Context, r coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
		require.True(t, held)
		require.Equal(t, receipt.Request, r)
		steps = append(steps, "store")
		return receipt, nil
	})
	adopter, err := coordination.NewTaskRecoveryAdopter(storage, fence, auth)
	require.NoError(t, err)
	got, err := adopter.Adopt(ctx, receipt.Request)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	require.Equal(t, []string{"fence", "authenticate proof and authorize", "store", "release"}, steps)
}

func TestTaskAdopterDenialAndFenceFailures(t *testing.T) {
	for _, mode := range []string{"denied", "changed proof", "missing callback", "duplicate callback", "lost ack", "missing proof", "bad receipt", "scope", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			receipt := taskAcceptanceFixture(t, true)
			req := receipt.Request
			ctx := cortex.WithScope(t.Context(), req.Current.Scope)
			calls := 0
			denied := errors.New("untrusted recovery proof")
			if mode == "scope" {
				ctx = context.Background()
			}
			if mode == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if mode == "missing proof" {
				req.Recovery = nil
			}
			fence := taskRecoveryFenceFunc(func(_ context.Context, r coordination.TaskAcceptanceRequest, next func(coordination.TaskAcceptanceRequest) error) error {
				switch mode {
				case "changed proof":
					r.Recovery.ProofRef = "different"
				case "missing callback":
					return nil
				case "duplicate callback":
					_ = next(r)
					_ = next(r)
					return nil
				case "lost ack":
					_ = next(r)
					return denied
				}
				return next(r)
			})
			auth := taskRecoveryAuthorizeFunc(func(context.Context, coordination.TaskAcceptanceRequest) error {
				if mode == "denied" {
					return denied
				}
				return nil
			})
			storage := taskRecoveryStorageFunc(func(context.Context, coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
				calls++
				if mode == "bad receipt" {
					receipt.Claim.RecoveryDigest = ""
				}
				return receipt, nil
			})
			adopter, err := coordination.NewTaskRecoveryAdopter(storage, fence, auth)
			require.NoError(t, err)
			got, err := adopter.Adopt(ctx, req)
			require.Error(t, err)
			require.Empty(t, got)
			if mode == "duplicate callback" || mode == "lost ack" || mode == "bad receipt" {
				require.Equal(t, 1, calls)
			} else {
				require.Zero(t, calls)
			}
		})
	}
}

func TestTaskAdopterRequiresPortsAndOrdinaryAcceptanceRejectsRecovery(t *testing.T) {
	receipt := taskAcceptanceFixture(t, true)
	s := taskRecoveryStorageFunc(func(context.Context, coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
		t.Fatal("unexpected adoption")
		return coordination.TaskAcceptance{}, nil
	})
	f := taskRecoveryFenceFunc(func(context.Context, coordination.TaskAcceptanceRequest, func(coordination.TaskAcceptanceRequest) error) error {
		return nil
	})
	a := taskRecoveryAuthorizeFunc(func(context.Context, coordination.TaskAcceptanceRequest) error { return nil })
	for _, ports := range []struct {
		s coordination.TaskRecoveryStore
		f coordination.TaskRecoveryFence
		a coordination.TaskRecoveryAuthorizer
	}{{nil, f, a}, {s, nil, a}, {s, f, nil}} {
		_, err := coordination.NewTaskRecoveryAdopter(ports.s, ports.f, ports.a)
		require.Error(t, err)
	}
	ordinary, err := coordination.NewTaskAcceptor(taskAcceptanceStorage{accept: func(context.Context, coordination.TaskAcceptanceRequest) (coordination.TaskAcceptance, error) {
		t.Fatal("ordinary acceptor reached storage")
		return receipt, nil
	}}, taskAcceptanceFenceFunc(func(context.Context, coordination.TaskAcceptanceRequest, func(coordination.TaskAcceptanceRequest) error) error {
		t.Fatal("ordinary acceptor entered fence")
		return nil
	}), taskAcceptanceAuthorizeFunc(func(context.Context, coordination.TaskAcceptanceRequest) error { return nil }))
	require.NoError(t, err)
	_, err = ordinary.Accept(cortex.WithScope(t.Context(), receipt.Request.Current.Scope), receipt.Request)
	require.Error(t, err)
}
