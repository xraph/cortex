package verification_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

type commitFenceFunc func(context.Context, verification.Binding, func(verification.Binding) error) error

func (f commitFenceFunc) WithCommitFence(ctx context.Context, b verification.Binding, commit func(verification.Binding) error) error {
	return f(ctx, b, commit)
}

type bindingAuthorizeFunc func(context.Context, verification.Binding) error

func (f bindingAuthorizeFunc) AuthorizeVerification(ctx context.Context, b verification.Binding) error {
	return f(ctx, b)
}

type commitStorage struct {
	verification.CommitStore
	commit func(context.Context, verification.CommitRequest) (verification.CommitReceipt, error)
}

func (s commitStorage) CommitVerifiedResult(ctx context.Context, req verification.CommitRequest) (verification.CommitReceipt, error) {
	return s.commit(ctx, req)
}

func committerFixture(t *testing.T) (context.Context, verification.CommitRequest, verification.CommitReceipt) {
	t.Helper()
	c, r, now := fixture(t)
	req := verification.CommitRequest{SchemaVersion: 1, ResultID: id.New(verification.PrefixResult), ExpectedRunRevision: 1, ContractID: c.ID, ReportIDs: []id.ID{r.ID}, Current: r.Binding}
	req, digest, err := req.Freeze()
	require.NoError(t, err)
	bindingDigest, err := req.Current.Digest()
	require.NoError(t, err)
	receipt, err := verification.SealCommitReceipt(verification.CommitReceipt{SchemaVersion: 1, Request: req, RequestDigest: digest, RunRevision: 2, ReportDigests: []string{r.Digest}, ObservedAt: now, Outbox: verification.CommitOutbox{EventID: id.New(verification.PrefixCommitEvent), Type: "result_committed", RunID: req.Current.RunID, ResultID: req.ResultID, BindingDigest: bindingDigest}})
	require.NoError(t, err)
	return cortex.WithScope(context.Background(), req.Current.Scope), req, receipt
}

func TestCommitterHoldsFenceThroughStorage(t *testing.T) {
	ctx, req, receipt := committerFixture(t)
	var held atomic.Bool
	var steps []string
	fence := commitFenceFunc(func(_ context.Context, current verification.Binding, commit func(verification.Binding) error) error {
		held.Store(true)
		steps = append(steps, "acquire")
		defer func() { held.Store(false); steps = append(steps, "release") }()
		return commit(current)
	})
	authorizer := bindingAuthorizeFunc(func(_ context.Context, _ verification.Binding) error {
		require.True(t, held.Load())
		steps = append(steps, "authorize")
		return nil
	})
	storage := commitStorage{commit: func(_ context.Context, got verification.CommitRequest) (verification.CommitReceipt, error) {
		require.True(t, held.Load())
		require.Equal(t, req, got)
		steps = append(steps, "store")
		return receipt, nil
	}}
	committer, err := verification.NewCommitter(storage, fence, authorizer)
	require.NoError(t, err)
	got, err := committer.Commit(ctx, req)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	require.Equal(t, []string{"acquire", "authorize", "store", "release"}, steps)
}

func TestCommitterDeniedOrStaleNeverWrites(t *testing.T) {
	denied := errors.New("revoked")
	for _, mode := range []string{"denied", "stale", "scope", "cancelled", "mutated scope"} {
		t.Run(mode, func(t *testing.T) {
			ctx, req, _ := committerFixture(t)
			calls := 0
			storage := commitStorage{commit: func(context.Context, verification.CommitRequest) (verification.CommitReceipt, error) {
				calls++
				return verification.CommitReceipt{}, nil
			}}
			fence := commitFenceFunc(func(_ context.Context, current verification.Binding, commit func(verification.Binding) error) error {
				if mode == "stale" {
					current.Context.AuthorityRevision = "revoked"
				}
				if mode == "mutated scope" {
					current.Scope.Levels[0].Value = "other"
				}
				return commit(current)
			})
			authorizer := bindingAuthorizeFunc(func(context.Context, verification.Binding) error {
				if mode == "denied" {
					return denied
				}
				return nil
			})
			if mode == "scope" {
				ctx = context.Background()
			}
			if mode == "cancelled" {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			committer, err := verification.NewCommitter(storage, fence, authorizer)
			require.NoError(t, err)
			got, err := committer.Commit(ctx, req)
			require.Error(t, err)
			if mode == "denied" {
				require.ErrorIs(t, err, denied)
			}
			require.Zero(t, got)
			require.Zero(t, calls)
		})
	}
}

func TestCommitterFreezesInputsBeforeHostCallbacks(t *testing.T) {
	ctx, req, receipt := committerFixture(t)
	expected, _, err := req.Freeze()
	require.NoError(t, err)
	fence := commitFenceFunc(func(_ context.Context, current verification.Binding, commit func(verification.Binding) error) error {
		req.ReportIDs[0] = id.New(verification.PrefixReport)
		req.Current.Scope.Levels[0].Value = "caller mutation"
		return commit(current)
	})
	authorizer := bindingAuthorizeFunc(func(_ context.Context, current verification.Binding) error {
		current.Scope.Levels[0].Value = "policy mutation"
		return nil
	})
	storage := commitStorage{commit: func(_ context.Context, got verification.CommitRequest) (verification.CommitReceipt, error) {
		require.Equal(t, expected, got)
		return receipt, nil
	}}
	committer, err := verification.NewCommitter(storage, fence, authorizer)
	require.NoError(t, err)
	_, err = committer.Commit(ctx, req)
	require.NoError(t, err)
}

func TestCommitterRejectsInvalidFenceProtocol(t *testing.T) {
	for _, mode := range []string{"never called", "twice", "late", "suppressed error"} {
		t.Run(mode, func(t *testing.T) {
			ctx, req, receipt := committerFixture(t)
			var retained func(verification.Binding) error
			var calls atomic.Int32
			denied := errors.New("denied")
			fence := commitFenceFunc(func(_ context.Context, current verification.Binding, commit func(verification.Binding) error) error {
				switch mode {
				case "never called":
					return nil
				case "late":
					retained = commit
					return nil
				case "twice":
					_ = commit(current)
					_ = commit(current)
					return nil
				default:
					_ = commit(current)
					return nil
				}
			})
			authorizer := bindingAuthorizeFunc(func(context.Context, verification.Binding) error {
				if mode == "suppressed error" {
					return denied
				}
				return nil
			})
			storage := commitStorage{commit: func(context.Context, verification.CommitRequest) (verification.CommitReceipt, error) {
				calls.Add(1)
				return receipt, nil
			}}
			committer, err := verification.NewCommitter(storage, fence, authorizer)
			require.NoError(t, err)
			got, err := committer.Commit(ctx, req)
			require.Error(t, err)
			require.Zero(t, got)
			if mode == "suppressed error" {
				require.ErrorIs(t, err, denied)
			}
			if retained != nil {
				require.Error(t, retained(req.Current))
			}
			if mode == "twice" {
				require.Equal(t, int32(1), calls.Load())
			} else {
				require.Zero(t, calls.Load())
			}
		})
	}
}

func TestCommitterReturnsNoReceiptOnStorageOrFenceFailure(t *testing.T) {
	for _, mode := range []string{"storage", "fence after commit", "invalid receipt", "wrong receipt"} {
		t.Run(mode, func(t *testing.T) {
			ctx, req, receipt := committerFixture(t)
			failure := errors.New("failed")
			storage := commitStorage{commit: func(context.Context, verification.CommitRequest) (verification.CommitReceipt, error) {
				switch mode {
				case "storage":
					return receipt, failure
				case "invalid receipt":
					receipt.Digest = "tampered"
				case "wrong receipt":
					_, _, receipt = committerFixture(t)
				}
				return receipt, nil
			}}
			fence := commitFenceFunc(func(_ context.Context, current verification.Binding, commit func(verification.Binding) error) error {
				err := commit(current)
				if mode == "fence after commit" {
					return failure
				}
				return err
			})
			committer, err := verification.NewCommitter(storage, fence, bindingAuthorizeFunc(func(context.Context, verification.Binding) error { return nil }))
			require.NoError(t, err)
			got, err := committer.Commit(ctx, req)
			require.Error(t, err)
			require.Zero(t, got)
		})
	}
}

func TestCommitterCancelsAnEscapedCallback(t *testing.T) {
	ctx, req, _ := committerFixture(t)
	entered := make(chan struct{})
	done := make(chan struct{})
	var wait sync.WaitGroup
	storage := commitStorage{commit: func(ctx context.Context, _ verification.CommitRequest) (verification.CommitReceipt, error) {
		close(entered)
		<-ctx.Done()
		close(done)
		return verification.CommitReceipt{}, ctx.Err()
	}}
	fence := commitFenceFunc(func(_ context.Context, current verification.Binding, commit func(verification.Binding) error) error {
		wait.Add(1)
		go func() { defer wait.Done(); _ = commit(current) }()
		<-entered
		return nil
	})
	committer, err := verification.NewCommitter(storage, fence, bindingAuthorizeFunc(func(context.Context, verification.Binding) error { return nil }))
	require.NoError(t, err)
	receipt, err := committer.Commit(ctx, req)
	require.Error(t, err)
	require.Zero(t, receipt)
	<-done
	wait.Wait()
}

func TestCommitterRequiresAllPorts(t *testing.T) {
	storage := commitStorage{}
	fence := commitFenceFunc(func(context.Context, verification.Binding, func(verification.Binding) error) error { return nil })
	authorizer := bindingAuthorizeFunc(func(context.Context, verification.Binding) error { return nil })
	var absentStorage *commitStorage
	var absentFence commitFenceFunc
	var absentAuthorizer bindingAuthorizeFunc
	for _, ports := range []struct {
		storage    verification.CommitStore
		fence      verification.CommitFence
		authorizer verification.BindingAuthorizer
	}{
		{nil, fence, authorizer}, {storage, nil, authorizer}, {storage, fence, nil},
		{absentStorage, fence, authorizer}, {storage, absentFence, authorizer}, {storage, fence, absentAuthorizer},
	} {
		committer, err := verification.NewCommitter(ports.storage, ports.fence, ports.authorizer)
		require.ErrorIs(t, err, verification.ErrInvalid)
		require.Nil(t, committer)
	}
}

func TestCommitterPreservesCancellationAfterAuthorization(t *testing.T) {
	ctx, req, _ := committerFixture(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	calls := 0
	storage := commitStorage{commit: func(context.Context, verification.CommitRequest) (verification.CommitReceipt, error) {
		calls++
		return verification.CommitReceipt{}, nil
	}}
	fence := commitFenceFunc(func(_ context.Context, current verification.Binding, commit func(verification.Binding) error) error {
		return commit(current)
	})
	authorizer := bindingAuthorizeFunc(func(context.Context, verification.Binding) error { cancel(); return nil })
	committer, err := verification.NewCommitter(storage, fence, authorizer)
	require.NoError(t, err)
	receipt, err := committer.Commit(ctx, req)
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, receipt)
	require.Zero(t, calls)
}
