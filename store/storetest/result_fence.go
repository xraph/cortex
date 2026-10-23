package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/verification"
)

// This fixture models a host whose policy writers share one guard. Production
// hosts must also cover other processes and their own policy persistence.
type resultFencePolicy struct {
	mu      sync.Mutex
	binding verification.Binding
	revoked bool
	denied  error
}

func (p *resultFencePolicy) WithCommitFence(ctx context.Context, _ verification.Binding, commit func(verification.Binding) error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	return commit(p.binding)
}

func (p *resultFencePolicy) AuthorizeVerification(_ context.Context, _ verification.Binding) error {
	// The committer must call this while WithCommitFence owns the policy guard.
	if p.revoked {
		return p.denied
	}
	return nil
}

type heldResultStorage struct {
	verification.CommitStore
	entered chan struct{}
	proceed chan struct{}
}

func (s heldResultStorage) CommitVerifiedResult(ctx context.Context, req verification.CommitRequest) (verification.CommitReceipt, error) {
	close(s.entered)
	select {
	case <-ctx.Done():
		return verification.CommitReceipt{}, ctx.Err()
	case <-s.proceed:
		return s.CommitStore.CommitVerifiedResult(ctx, req)
	}
}

func testVerifiedResultHostFence(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req := resultFixture(t, s)
	raw := resultCapability[verification.CommitStore](t, s)
	policy := &resultFencePolicy{binding: req.Current, denied: errors.New("host grant revoked")}
	storage := heldResultStorage{CommitStore: raw, entered: make(chan struct{}), proceed: make(chan struct{})}
	committer, err := verification.NewCommitter(storage, policy, policy)
	require.NoError(t, err)
	type outcome struct {
		receipt verification.CommitReceipt
		err     error
	}
	completed := make(chan outcome, 1)
	go func() { receipt, commitErr := committer.Commit(ctx, req); completed <- outcome{receipt, commitErr} }()
	<-storage.entered
	// Check lock ownership directly instead of inferring it from a short timeout.
	if policy.mu.TryLock() {
		policy.mu.Unlock()
		t.Fatal("host guard was released before storage")
	}
	revoking, revoked := make(chan struct{}), make(chan struct{})
	go func() {
		close(revoking)
		policy.mu.Lock()
		policy.revoked = true
		policy.mu.Unlock()
		close(revoked)
	}()
	<-revoking
	close(storage.proceed)
	result := <-completed
	require.NoError(t, result.err)
	require.NoError(t, result.receipt.Validate())
	<-revoked
	// Raw storage retains the historical receipt, but the guarded API must deny
	// the retry before it reaches storage and exposes that receipt again.
	retry, err := committer.Commit(ctx, req)
	require.ErrorIs(t, err, policy.denied)
	require.Zero(t, retry)
	saved, err := raw.GetCommittedResult(ctx, req.ResultID)
	require.NoError(t, err)
	require.Equal(t, result.receipt, saved)
	current, err := s.GetRun(ctx, req.Current.RunID)
	require.NoError(t, err)
	require.Equal(t, req.ExpectedRunRevision+1, current.Revision)
}
