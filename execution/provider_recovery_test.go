package execution_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
)

type providerProbe func(context.Context, execution.RunCallRequest) (execution.ProviderCallReceipt, error)

func (f providerProbe) ReconcileProviderCall(ctx context.Context, r execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
	return f(ctx, r)
}

type providerFence func(context.Context, execution.RunCallRequest, func() error) error

func (f providerFence) WithProviderCallFence(ctx context.Context, r execution.RunCallRequest, commit func() error) error {
	return f(ctx, r, commit)
}

type providerReceiptMemory struct {
	execution.RunCallStore
	record  execution.RunCallRecord
	receipt *execution.ProviderCallReceipt
	writes  int
	loseAck bool
}

func (s *providerReceiptMemory) GetRunCall(context.Context, cortex.ID, string) (execution.RunCallRecord, error) {
	return s.record, nil
}
func (s *providerReceiptMemory) GetProviderCallReceipt(context.Context, execution.RunCallRequest) (*execution.ProviderCallReceipt, error) {
	return s.receipt, nil
}
func (s *providerReceiptMemory) PutProviderCallReceipt(_ context.Context, _ execution.RunCallRequest, c execution.ProviderCallReceipt) error {
	s.writes++
	s.receipt = &c
	if s.loseAck {
		return errors.New("lost acknowledgment")
	}
	return nil
}

func recoveryFixture(t *testing.T) (context.Context, execution.RunCallRequest, *providerReceiptMemory, execution.ProviderRecoveryConfig, *atomic.Int32) {
	t.Helper()
	r := runCallRequest()
	c := providerReceipt(t, r)
	s := &providerReceiptMemory{record: execution.RunCallRecord{Request: r, StartedAt: c.ObservedAt.Add(-time.Second)}}
	calls := &atomic.Int32{}
	config := execution.ProviderRecoveryConfig{
		Store: s,
		Adapter: providerProbe(func(context.Context, execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
			calls.Add(1)
			return c, nil
		}),
		Authorize:    func(context.Context, execution.RunCallRequest) error { return nil },
		Authenticate: func(context.Context, execution.RunCallRequest, execution.ProviderCallReceipt) error { return nil },
		Fence:        providerFence(func(_ context.Context, _ execution.RunCallRequest, commit func() error) error { return commit() }),
	}
	return cortex.WithScope(t.Context(), r.Scope), r, s, config, calls
}

func TestProviderRecoveryRetainsProofWithoutInventingLocalReturn(t *testing.T) {
	ctx, r, s, config, calls := recoveryFixture(t)
	auths, proofs := 0, 0
	config.Authorize = func(context.Context, execution.RunCallRequest) error { auths++; return nil }
	config.Authenticate = func(context.Context, execution.RunCallRequest, execution.ProviderCallReceipt) error {
		proofs++
		return nil
	}
	service, err := execution.NewProviderReconciler(config)
	require.NoError(t, err)
	got, err := service.Reconcile(ctx, r)
	require.NoError(t, err)
	require.Equal(t, execution.ProviderCallSucceeded, got.Outcome)
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, 1, s.writes)
	require.Nil(t, s.record.Completion)
	again, err := service.Reconcile(ctx, r)
	require.NoError(t, err)
	require.Equal(t, got, again)
	require.Equal(t, int32(1), calls.Load(), "stored proof avoids another provider probe")
	require.Equal(t, 1, s.writes)
	require.Equal(t, 4, auths, "historical proof needs current permission")
	require.Equal(t, 2, proofs, "historical proof needs current authentication")
	s.record.Completion = &execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: got.RequestDigest, ResultDigest: evidence.Digest([]byte("late conflicting output"))}
	s.record.CompletedAt = got.ObservedAt
	_, err = service.Reconcile(ctx, r)
	require.ErrorIs(t, err, execution.ErrRunCallConflict)
	require.Equal(t, int32(1), calls.Load())
}

func TestProviderRecoveryFailsClosed(t *testing.T) {
	denied := errors.New("denied")
	for _, name := range []string{"authorization", "revoked", "proof", "unknown", "binding", "local conflict", "wrong original", "missing fence callback", "double fence callback", "cancelled adapter", "scope"} {
		t.Run(name, func(t *testing.T) {
			ctx, r, s, config, calls := recoveryFixture(t)
			expected := denied
			switch name {
			case "authorization":
				config.Authorize = func(context.Context, execution.RunCallRequest) error { return denied }
			case "revoked":
				count := 0
				config.Authorize = func(context.Context, execution.RunCallRequest) error {
					count++
					if count > 1 {
						return denied
					}
					return nil
				}
			case "proof":
				config.Authenticate = func(context.Context, execution.RunCallRequest, execution.ProviderCallReceipt) error { return denied }
			case "unknown":
				config.Adapter = providerProbe(func(context.Context, execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
					return execution.ProviderCallReceipt{}, execution.ErrProviderCallUncertain
				})
				expected = execution.ErrProviderCallUncertain
			case "binding":
				c := providerReceipt(t, r)
				c.RequestDigest = r.RequestDigest
				config.Adapter = providerProbe(func(context.Context, execution.RunCallRequest) (execution.ProviderCallReceipt, error) { return c, nil })
				expected = execution.ErrInvalidRunCall
			case "local conflict":
				c := providerReceipt(t, r)
				s.record.Completion = &execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: c.RequestDigest, ResultDigest: evidence.Digest([]byte("wrong"))}
				s.record.CompletedAt = c.ObservedAt
				expected = execution.ErrRunCallConflict
			case "wrong original":
				s.record.Request.PreparedDigest = evidence.Digest([]byte("changed"))
				expected = execution.ErrRunCallConflict
			case "missing fence callback":
				config.Fence = providerFence(func(context.Context, execution.RunCallRequest, func() error) error { return nil })
				expected = execution.ErrInvalidRunCall
			case "double fence callback":
				config.Fence = providerFence(func(_ context.Context, _ execution.RunCallRequest, commit func() error) error {
					_ = commit()
					return commit()
				})
				expected = execution.ErrInvalidRunCall
			case "cancelled adapter":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				defer cancel()
				c := providerReceipt(t, r)
				config.Adapter = providerProbe(func(context.Context, execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
					cancel()
					return c, nil
				})
				expected = context.Canceled
			case "scope":
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
				expected = cortex.ErrInvalidScope
			}
			service, err := execution.NewProviderReconciler(config)
			require.NoError(t, err)
			got, err := service.Reconcile(ctx, r)
			require.ErrorIs(t, err, expected)
			require.Empty(t, got)
			if name != "double fence callback" {
				require.Zero(t, s.writes)
			}
			if name == "authorization" || name == "scope" || name == "wrong original" {
				require.Zero(t, calls.Load())
			}
		})
	}
}

func TestProviderRecoveryLostAcknowledgmentUsesDurableProof(t *testing.T) {
	ctx, r, s, config, calls := recoveryFixture(t)
	s.loseAck = true
	service, err := execution.NewProviderReconciler(config)
	require.NoError(t, err)
	_, err = service.Reconcile(ctx, r)
	require.Error(t, err)
	require.NotNil(t, s.receipt)
	s.loseAck = false
	_, err = service.Reconcile(ctx, r)
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())
	require.Equal(t, 1, s.writes)
}

func TestProviderRecoveryBoundsSlotsAndWaits(t *testing.T) {
	ctx, r, _, config, _ := recoveryFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	config.MaxConcurrent = 1
	config.Timeout = time.Second
	config.Adapter = providerProbe(func(ctx context.Context, _ execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return execution.ProviderCallReceipt{}, execution.ErrProviderCallUncertain
	})
	service, err := execution.NewProviderReconciler(config)
	require.NoError(t, err)
	first, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() { _, reconcileErr := service.Reconcile(first, r); done <- reconcileErr }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("probe did not start")
	}
	queued, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	_, err = service.Reconcile(queued, r)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	stop()
	require.ErrorIs(t, <-done, context.Canceled)
}

func TestProviderRecoveryRequiresEveryHostBoundary(t *testing.T) {
	for _, name := range []string{"store", "adapter", "authorize", "authenticate", "fence", "slots", "timeout"} {
		t.Run(name, func(t *testing.T) {
			_, _, _, config, _ := recoveryFixture(t)
			switch name {
			case "store":
				config.Store = (*providerReceiptMemory)(nil)
			case "adapter":
				config.Adapter = providerProbe(nil)
			case "authorize":
				config.Authorize = nil
			case "authenticate":
				config.Authenticate = nil
			case "fence":
				config.Fence = providerFence(nil)
			case "slots":
				config.MaxConcurrent = -1
			case "timeout":
				config.Timeout = time.Hour
			}
			_, err := execution.NewProviderReconciler(config)
			require.ErrorIs(t, err, execution.ErrInvalidRunCall)
		})
	}
}

func TestProviderRecoveryDetachesRequestsAndRecoversExpiredCalls(t *testing.T) {
	ctx, r, s, config, _ := recoveryFixture(t)
	r.Deadline = time.Now().UTC().Add(-time.Minute)
	s.record.Request = r
	s.record.StartedAt = r.Deadline.Add(-time.Second)
	receipt := providerReceipt(t, r)
	tamper := func(request execution.RunCallRequest) { request.Scope.Levels[0].Value = "mutated" }
	config.Authorize = func(_ context.Context, request execution.RunCallRequest) error { tamper(request); return nil }
	config.Authenticate = func(_ context.Context, request execution.RunCallRequest, _ execution.ProviderCallReceipt) error {
		tamper(request)
		return nil
	}
	config.Fence = providerFence(func(_ context.Context, request execution.RunCallRequest, commit func() error) error {
		tamper(request)
		return commit()
	})
	config.Adapter = providerProbe(func(_ context.Context, request execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
		tamper(request)
		return receipt, nil
	})
	service, err := execution.NewProviderReconciler(config)
	require.NoError(t, err)
	got, err := service.Reconcile(ctx, r)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	require.Equal(t, "one", r.Scope.Levels[0].Value)
	require.Equal(t, r, s.record.Request)
}

func TestProviderRecoveryEnforcesOperationTimeout(t *testing.T) {
	ctx, r, s, config, _ := recoveryFixture(t)
	config.Timeout = 20 * time.Millisecond
	config.Adapter = providerProbe(func(ctx context.Context, _ execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
		<-ctx.Done()
		return execution.ProviderCallReceipt{}, ctx.Err()
	})
	service, err := execution.NewProviderReconciler(config)
	require.NoError(t, err)
	_, err = service.Reconcile(ctx, r)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Zero(t, s.writes)
}

func TestProviderRecoveryRetainsTerminalOutcomesForCompleteAndStream(t *testing.T) {
	for _, kind := range []execution.RunCallKind{execution.RunCallComplete, execution.RunCallStream} {
		for _, outcome := range []execution.ProviderCallOutcome{execution.ProviderCallSucceeded, execution.ProviderCallFailed, execution.ProviderCallNotAccepted} {
			t.Run(string(kind)+"/"+string(outcome), func(t *testing.T) {
				ctx, r, s, config, _ := recoveryFixture(t)
				r.Kind = kind
				s.record.Request = r
				c := providerReceipt(t, r)
				c.Outcome = outcome
				if outcome != execution.ProviderCallSucceeded {
					c.ResultDigest = ""
				}
				if outcome == execution.ProviderCallNotAccepted {
					c.Usage = execution.ReservationUsage{}
				}
				config.Adapter = providerProbe(func(context.Context, execution.RunCallRequest) (execution.ProviderCallReceipt, error) { return c, nil })
				service, err := execution.NewProviderReconciler(config)
				require.NoError(t, err)
				got, err := service.Reconcile(ctx, r)
				require.NoError(t, err)
				require.Equal(t, c, got)
				require.Equal(t, 1, s.writes)
				require.Nil(t, s.record.Completion)
			})
		}
	}
}
