package execution

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

func effectRequest() EffectRequest {
	return EffectRequest{SchemaVersion: 1, Scope: cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "effects"}}}, RunID: id.NewAgentRunID(), OperationKey: "publish-one", AttemptID: id.New(PrefixEffectAttempt), ArgsDigest: evidence.Digest([]byte("payload")), PrincipalRef: "worker", AuthorityRef: "grant-v1", Deadline: time.Now().UTC().Add(time.Minute), Requirement: EffectRequirement{Tool: "publish", Revision: "1", Class: EffectNonIdempotent, ReconcileRevision: "probe-1", Retry: RetryPolicy{MaxAttempts: 1}}}
}

type effectMemoryStore struct {
	mu           sync.Mutex
	request      *EffectRequest
	receipt      *EffectReceipt
	loseAck      bool
	loseBeginAck bool
}

func (s *effectMemoryStore) BeginEffect(_ context.Context, r EffectRequest) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.request != nil {
		_, a, _ := s.request.Freeze()
		_, b, _ := r.Freeze()
		if a != b {
			return false, ErrEffectConflict
		}
		return false, nil
	}
	s.request = &r
	if s.loseBeginAck {
		s.loseBeginAck = false
		return false, errors.New("lost reservation ack")
	}
	return true, nil
}
func (s *effectMemoryStore) GetEffect(_ context.Context, _ string) (EffectRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.request == nil {
		return EffectRecord{}, ErrEffectNotFound
	}
	return EffectRecord{Request: *s.request, Receipt: s.receipt}, nil
}
func (s *effectMemoryStore) CompleteEffect(_ context.Context, _ EffectRequest, v EffectReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipt = &v
	if s.loseAck {
		s.loseAck = false
		return errors.New("lost completion ack")
	}
	return nil
}

type effectFixture struct {
	calls, probes int
	loseAck       bool
	unknown       bool
}

func (f *effectFixture) Execute(_ context.Context, r EffectRequest, _ []byte) (EffectReceipt, error) {
	f.calls++
	if f.loseAck {
		return EffectReceipt{}, errors.New("lost provider ack")
	}
	return effectApplied(r), nil
}
func (f *effectFixture) Reconcile(_ context.Context, r EffectRequest) (EffectStatus, error) {
	f.probes++
	if f.unknown {
		return EffectStatus{Outcome: EffectUnknown}, nil
	}
	v := effectApplied(r)
	return EffectStatus{Outcome: EffectApplied, Receipt: &v}, nil
}
func effectApplied(r EffectRequest) EffectReceipt {
	_, digest, _ := r.Freeze()
	return EffectReceipt{SchemaVersion: 1, RequestDigest: digest, Outcome: EffectApplied, ExternalID: "external-1", ExternalVersion: "v1", EvidenceRef: "provider-receipt-1"}
}
func effectExecutor(t *testing.T, s EffectStore, f *effectFixture, r EffectRequest, authorize func(context.Context, EffectRequest, EffectAction) error) *EffectExecutor {
	t.Helper()
	if authorize == nil {
		authorize = func(context.Context, EffectRequest, EffectAction) error { return nil }
	}
	e, err := NewEffectExecutor(EffectExecutorConfig{Store: s, Adapters: []EffectRegistration{{Requirement: r.Requirement, Adapter: f}}, Authorize: authorize, MaxConcurrent: 1})
	require.NoError(t, err)
	return e
}

func TestCrashAfterExternalEffectReconciles(t *testing.T) {
	r := effectRequest()
	ctx := cortex.WithScope(context.Background(), r.Scope)
	s := &effectMemoryStore{}
	f := &effectFixture{loseAck: true}
	e := effectExecutor(t, s, f, r, nil)
	_, err := e.Execute(ctx, r, []byte("payload"))
	require.ErrorIs(t, err, ErrEffectUncertain)
	require.Equal(t, 1, f.calls)
	e = effectExecutor(t, s, f, r, nil)
	v, err := e.Execute(ctx, r, []byte("payload"))
	require.NoError(t, err)
	require.Equal(t, EffectApplied, v.Outcome)
	require.Equal(t, 1, f.calls)
	require.Equal(t, 1, f.probes)
	_, err = e.Execute(ctx, r, []byte("payload"))
	require.NoError(t, err)
	require.Equal(t, 1, f.calls)
	require.Equal(t, 1, f.probes)
}
func TestUnknownNonIdempotentEffectIsNotRetried(t *testing.T) {
	r := effectRequest()
	ctx := cortex.WithScope(context.Background(), r.Scope)
	s := &effectMemoryStore{}
	f := &effectFixture{loseAck: true, unknown: true}
	e := effectExecutor(t, s, f, r, nil)
	for range 3 {
		_, err := e.Execute(ctx, r, []byte("payload"))
		require.ErrorIs(t, err, ErrEffectUncertain)
	}
	require.Equal(t, 1, f.calls)
	require.Equal(t, 2, f.probes)
}
func TestEffectChecksAuthorityAndArgumentsBeforeDispatch(t *testing.T) {
	for _, mode := range []string{"denied", "arguments", "classification", "scope", "expired", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			r := effectRequest()
			original := r
			ctx := cortex.WithScope(context.Background(), r.Scope)
			s := &effectMemoryStore{}
			f := &effectFixture{}
			authorize := func(context.Context, EffectRequest, EffectAction) error { return nil }
			args := []byte("payload")
			switch mode {
			case "denied":
				authorize = func(context.Context, EffectRequest, EffectAction) error { return errors.New("denied") }
			case "arguments":
				args = []byte("different")
			case "classification":
				r.Requirement.Class = EffectReadOnly
			case "scope":
				ctx = context.Background()
			case "expired":
				r.Deadline = time.Now().Add(-time.Second)
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			e := effectExecutor(t, s, f, original, authorize)
			_, err := e.Execute(ctx, r, args)
			require.Error(t, err)
			require.Zero(t, f.calls)
			require.Nil(t, s.request)
		})
	}
}
func TestEffectCompletionLostAcknowledgementDoesNotRepeat(t *testing.T) {
	r := effectRequest()
	ctx := cortex.WithScope(context.Background(), r.Scope)
	s := &effectMemoryStore{loseAck: true}
	f := &effectFixture{}
	e := effectExecutor(t, s, f, r, nil)
	_, err := e.Execute(ctx, r, []byte("payload"))
	require.ErrorIs(t, err, ErrEffectUncertain)
	_, err = e.Execute(ctx, r, []byte("payload"))
	require.NoError(t, err)
	require.Equal(t, 1, f.calls)
	require.Zero(t, f.probes)
}

func TestEffectReservationLostAcknowledgementIsUncertain(t *testing.T) {
	r := effectRequest()
	ctx := cortex.WithScope(context.Background(), r.Scope)
	s := &effectMemoryStore{loseBeginAck: true}
	f := &effectFixture{unknown: true}
	e := effectExecutor(t, s, f, r, nil)
	_, err := e.Execute(ctx, r, []byte("payload"))
	require.ErrorIs(t, err, ErrEffectUncertain)
	require.Zero(t, f.calls)
	_, err = e.Execute(ctx, r, []byte("payload"))
	require.ErrorIs(t, err, ErrEffectUncertain)
	require.Zero(t, f.calls)
	require.Equal(t, 1, f.probes)
}
func TestEffectExpiredRequestCanStillReconcile(t *testing.T) {
	r := effectRequest()
	r.Deadline = time.Now().UTC().Add(-time.Second)
	ctx := cortex.WithScope(context.Background(), r.Scope)
	s := &effectMemoryStore{}
	_, err := s.BeginEffect(ctx, r)
	require.NoError(t, err)
	f := &effectFixture{}
	e := effectExecutor(t, s, f, r, nil)
	_, err = e.Execute(ctx, r, []byte("payload"))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	v, err := e.Reconcile(ctx, r)
	require.NoError(t, err)
	require.Equal(t, EffectApplied, v.Outcome)
	require.Zero(t, f.calls)
}
func TestEffectAuthorityRevocationStopsDispatchAndCachedReceipt(t *testing.T) {
	r := effectRequest()
	ctx := cortex.WithScope(context.Background(), r.Scope)
	s := &effectMemoryStore{}
	f := &effectFixture{}
	checks := 0
	authorize := func(context.Context, EffectRequest, EffectAction) error {
		checks++
		if checks > 1 {
			return errors.New("revoked")
		}
		return nil
	}
	e := effectExecutor(t, s, f, r, authorize)
	_, err := e.Execute(ctx, r, []byte("payload"))
	require.ErrorIs(t, err, ErrEffectUncertain)
	require.NotNil(t, s.request)
	require.Zero(t, f.calls)
	s.receipt = new(effectApplied(r))
	_, err = e.Reconcile(ctx, r)
	require.Error(t, err)
	require.Zero(t, f.probes)
}
