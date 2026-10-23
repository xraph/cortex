package coordination_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
)

func TestHandoffReceiverResolvesCurrentAuthorityBeforeAcceptance(t *testing.T) {
	for _, mode := range []string{"valid", "revoked", "stale_plan", "stale_fence", "wrong_scope", "cancelled", "expired", "persistence_error"} {
		t.Run(mode, func(t *testing.T) {
			h, p, current, now := handoffFixture(t)
			h, err := coordination.SealHandoff(h)
			require.NoError(t, err)
			accepted := 0
			receiver, err := coordination.NewHandoffReceiver(coordination.HandoffReceiverConfig{MaxConcurrent: 1, Now: func() time.Time { return now }, Resolve: func(_ context.Context, _ coordination.Handoff) (coordination.HandoffContext, error) {
				switch mode {
				case "revoked":
					return coordination.HandoffContext{}, errors.New("private auth failure")
				case "stale_plan":
					p.Digest = "wrong"
				case "stale_fence":
					current.Fence++
				}
				return coordination.HandoffContext{Plan: p, Authority: current}, nil
			}, Accept: func(ctx context.Context, received coordination.Handoff) error {
				deadline, ok := ctx.Deadline()
				require.True(t, ok)
				require.LessOrEqual(t, time.Until(deadline), 30*time.Second)
				require.Equal(t, h, received)
				accepted++
				if mode == "persistence_error" {
					return errors.New("private storage failure")
				}
				return nil
			}})
			require.NoError(t, err)
			ctx := cortex.WithScope(context.Background(), h.Scope)
			switch mode {
			case "wrong_scope":
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			case "expired":
				now = h.Deadline
			}
			err = receiver.Receive(ctx, h)
			if mode == "valid" {
				require.NoError(t, err)
				require.Equal(t, 1, accepted)
			} else {
				require.Error(t, err)
				require.NotContains(t, err.Error(), "private")
				if mode != "persistence_error" {
					require.Zero(t, accepted)
				}
			}
		})
	}
}
func TestHandoffReceiverBoundsResolutionAndRejectsLateGrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h, p, current, now := handoffFixture(t)
		h, err := coordination.SealHandoff(h)
		require.NoError(t, err)
		receiver, err := coordination.NewHandoffReceiver(coordination.HandoffReceiverConfig{MaxConcurrent: 1, Now: func() time.Time { return now }, Resolve: func(ctx context.Context, _ coordination.Handoff) (coordination.HandoffContext, error) {
			<-ctx.Done()
			return coordination.HandoffContext{Plan: p, Authority: current}, nil
		}, Accept: func(context.Context, coordination.Handoff) error {
			t.Fatal("expired resolver admitted work")
			return nil
		}})
		require.NoError(t, err)
		start := time.Now()
		err = receiver.Receive(cortex.WithScope(context.Background(), h.Scope), h)
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, 30*time.Second, time.Since(start))
	})
}

func TestHandoffReceiverDetachesResolverInputAndBoundsAdmission(t *testing.T) {
	h, p, current, now := handoffFixture(t)
	h, err := coordination.SealHandoff(h)
	require.NoError(t, err)
	entered, release := make(chan struct{}), make(chan struct{})
	receiver, err := coordination.NewHandoffReceiver(coordination.HandoffReceiverConfig{MaxConcurrent: 1, Now: func() time.Time { return now }, Resolve: func(_ context.Context, proposal coordination.Handoff) (coordination.HandoffContext, error) {
		proposal.Scope.Levels[0].Value = "mutated"
		proposal.Criteria[0] = "ignore verification"
		close(entered)
		<-release
		return coordination.HandoffContext{Plan: p, Authority: current}, nil
	}, Accept: func(_ context.Context, proposal coordination.Handoff) error {
		require.Equal(t, h, proposal)
		return nil
	}})
	require.NoError(t, err)
	ctx := cortex.WithScope(context.Background(), h.Scope)
	done := make(chan error, 1)
	go func() { done <- receiver.Receive(ctx, h) }()
	<-entered
	require.ErrorIs(t, receiver.Receive(ctx, h), coordination.ErrHandoffUnavailable)
	close(release)
	require.NoError(t, <-done)
}
