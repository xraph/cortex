package execution_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func callSealRequest() execution.RunCallSealRequest {
	r := runCallRequest()
	return execution.RunCallSealRequest{SchemaVersion: 1, Scope: r.Scope, RunID: r.RunID, Actor: "recovery-service", Reason: "reconcile interrupted task"}
}

func TestRunCallSealBindsRunScopeAndActor(t *testing.T) {
	r := callSealRequest()
	frozen, digest, err := r.Freeze()
	require.NoError(t, err)
	r.Scope.Levels[0].Value = "changed"
	require.NotEqual(t, r.Scope, frozen.Scope)
	sealed := execution.RunCallSeal{Request: frozen, RequestDigest: digest, At: time.Now().UTC()}
	require.NoError(t, sealed.Validate())
	sealed.Request.Actor = "different"
	require.ErrorIs(t, sealed.Validate(), execution.ErrInvalidRunCall)
	for name, change := range map[string]func(*execution.RunCallSealRequest){
		"version": func(r *execution.RunCallSealRequest) { r.SchemaVersion = 0 },
		"scope":   func(r *execution.RunCallSealRequest) { r.Scope = cortex.Scope{} },
		"run":     func(r *execution.RunCallSealRequest) { r.RunID = id.NewAgentID() },
		"actor":   func(r *execution.RunCallSealRequest) { r.Actor = "" },
		"reason":  func(r *execution.RunCallSealRequest) { r.Reason = "" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := callSealRequest()
			change(&candidate)
			_, _, freezeErr := candidate.Freeze()
			require.ErrorIs(t, freezeErr, execution.ErrInvalidRunCall)
		})
	}
}

type sealFence func(context.Context, execution.RunCallSealRequest, func(execution.RunCallSealRequest) error) error

func (f sealFence) WithRunCallSeal(ctx context.Context, r execution.RunCallSealRequest, next func(execution.RunCallSealRequest) error) error {
	return f(ctx, r, next)
}

type sealAuthorizer func(context.Context, execution.RunCallSealRequest) error

func (f sealAuthorizer) AuthorizeRunCallSeal(ctx context.Context, r execution.RunCallSealRequest) error {
	return f(ctx, r)
}

type sealMemory struct {
	execution.RunCallSealStore
	calls  int
	change bool
}

func (s *sealMemory) SealRunCalls(_ context.Context, r execution.RunCallSealRequest) (execution.RunCallSeal, error) {
	s.calls++
	if s.change {
		r.Actor = "changed"
	}
	frozen, digest, err := r.Freeze()
	return execution.RunCallSeal{Request: frozen, RequestDigest: digest, At: time.Now().UTC()}, err
}

func TestRunCallSealerRequiresCurrentAuthority(t *testing.T) {
	for _, mode := range []string{"success", "denied", "changed request", "changed receipt", "missing callback", "repeated callback", "cancelled", "scope"} {
		t.Run(mode, func(t *testing.T) {
			r := callSealRequest()
			ctx := cortex.WithScope(t.Context(), r.Scope)
			s := &sealMemory{change: mode == "changed receipt"}
			denied := errors.New("denied")
			expected := error(nil)
			auths := 0
			authorizer := sealAuthorizer(func(_ context.Context, offered execution.RunCallSealRequest) error {
				auths++
				offered.Scope.Levels[0].Value = "host mutation"
				if mode == "denied" {
					return denied
				}
				return nil
			})
			fence := sealFence(func(_ context.Context, offered execution.RunCallSealRequest, next func(execution.RunCallSealRequest) error) error {
				switch mode {
				case "missing callback":
					return nil
				case "changed request":
					offered.Reason = "changed"
				case "repeated callback":
					_ = next(offered)
				}
				return next(offered)
			})
			switch mode {
			case "denied":
				expected = denied
			case "changed request":
				expected = execution.ErrRunCallConflict
			case "changed receipt", "missing callback", "repeated callback":
				expected = execution.ErrInvalidRunCall
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				expected = context.Canceled
			case "scope":
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
				expected = cortex.ErrInvalidScope
			}
			sealer, err := execution.NewRunCallSealer(s, fence, authorizer)
			require.NoError(t, err)
			receipt, err := sealer.Seal(ctx, r)
			if expected != nil {
				require.ErrorIs(t, err, expected)
				require.Empty(t, receipt)
			} else {
				require.NoError(t, err)
				require.NoError(t, receipt.Validate())
				require.Equal(t, r, receipt.Request)
				_, err = sealer.Seal(ctx, r)
				require.NoError(t, err)
				require.Equal(t, 2, auths)
			}
			if mode == "denied" || mode == "changed request" || mode == "missing callback" || mode == "cancelled" || mode == "scope" {
				require.Zero(t, s.calls)
			}
		})
	}
	for _, mode := range []string{"store", "fence", "authorizer"} {
		t.Run(mode, func(t *testing.T) {
			var s execution.RunCallSealStore = &sealMemory{}
			var f execution.RunCallSealFence = sealFence(func(_ context.Context, r execution.RunCallSealRequest, next func(execution.RunCallSealRequest) error) error {
				return next(r)
			})
			var a execution.RunCallSealAuthorizer = sealAuthorizer(func(context.Context, execution.RunCallSealRequest) error { return nil })
			switch mode {
			case "store":
				s = (*sealMemory)(nil)
			case "fence":
				f = sealFence(nil)
			case "authorizer":
				a = sealAuthorizer(nil)
			}
			_, err := execution.NewRunCallSealer(s, f, a)
			require.ErrorIs(t, err, execution.ErrInvalidRunCall)
		})
	}
}

func TestRunCallSealerRejectsEscapedCallback(t *testing.T) {
	r := callSealRequest()
	ctx := cortex.WithScope(t.Context(), r.Scope)
	s := &sealMemory{}
	var escaped func(execution.RunCallSealRequest) error
	fence := sealFence(func(_ context.Context, _ execution.RunCallSealRequest, next func(execution.RunCallSealRequest) error) error {
		escaped = next
		return nil
	})
	sealer, err := execution.NewRunCallSealer(s, fence, sealAuthorizer(func(context.Context, execution.RunCallSealRequest) error {
		t.Fatal("escaped callback reached authorization")
		return nil
	}))
	require.NoError(t, err)
	_, err = sealer.Seal(ctx, r)
	require.ErrorIs(t, err, execution.ErrInvalidRunCall)
	require.NotNil(t, escaped)
	require.ErrorIs(t, escaped(r), execution.ErrInvalidRunCall)
	require.Zero(t, s.calls)
}
