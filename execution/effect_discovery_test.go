package execution

import (
	"context"
	"errors"
	"sort"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

type discoveryEffectStore struct {
	records       []EffectRecord
	list          func(context.Context, EffectListRequest) ([]EffectRecord, error)
	begins, lists int
}

func (s *discoveryEffectStore) BeginEffect(context.Context, EffectRequest) (bool, error) {
	s.begins++
	return false, errors.New("recovery must not reserve")
}
func (s *discoveryEffectStore) ListEffects(ctx context.Context, r EffectListRequest) ([]EffectRecord, error) {
	s.lists++
	if s.list != nil {
		return s.list(ctx, r)
	}
	records := make([]EffectRecord, 0, 3)
	for _, record := range s.records {
		if effectCursor(record.Request) > r.After && (r.RunID.IsNil() || record.Request.RunID == r.RunID) {
			records = append(records, record)
			if len(records) == r.Limit {
				break
			}
		}
	}
	return records, nil
}
func (s *discoveryEffectStore) GetEffect(_ context.Context, key string) (EffectRecord, error) {
	for _, r := range s.records {
		if r.Request.OperationKey == key {
			return r, nil
		}
	}
	return EffectRecord{}, ErrEffectNotFound
}
func (s *discoveryEffectStore) CompleteEffect(_ context.Context, r EffectRequest, receipt EffectReceipt) error {
	for i := range s.records {
		if s.records[i].Request.OperationKey == r.OperationKey {
			s.records[i].Receipt = &receipt
			return nil
		}
	}
	return ErrEffectNotFound
}
func effectCursor(r EffectRequest) string { return evidence.Digest([]byte(r.OperationKey)) }
func discoveryRecords() []EffectRecord {
	r := effectRequest()
	records := make([]EffectRecord, 0, 3)
	for _, key := range []string{"one", "two", "three"} {
		next := cloneEffect(r)
		next.OperationKey = key
		next.AttemptID = id.New(PrefixEffectAttempt)
		next.Deadline = time.Now().Add(-time.Hour)
		records = append(records, EffectRecord{Request: next})
	}
	sort.Slice(records, func(i, j int) bool { return effectCursor(records[i].Request) < effectCursor(records[j].Request) })
	return records
}
func TestEffectRecoveryDiscoversOriginalAttemptsAndWraps(t *testing.T) {
	s := &discoveryEffectStore{records: discoveryRecords()}
	retained := effectApplied(s.records[0].Request)
	s.records[0].Receipt = &retained
	f := &effectFixture{}
	e := effectExecutor(t, s, f, s.records[0].Request, nil)
	ctx := cortex.WithScope(context.Background(), s.records[0].Request.Scope)
	page, err := e.RecoverPage(ctx, EffectListRequest{Limit: 2})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.Equal(t, effectCursor(s.records[1].Request), page.NextAfter)
	require.Equal(t, EffectRecoveryTerminal, page.Items[0].Status)
	require.Equal(t, EffectApplied, page.Items[1].Outcome)
	require.Equal(t, 1, f.probes, "retained receipt needs no provider probe")
	page, err = e.RecoverPage(ctx, EffectListRequest{Limit: 2, After: page.NextAfter})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Empty(t, page.NextAfter)
	require.Equal(t, 2, f.probes)
	require.Zero(t, f.calls)
	require.Zero(t, s.begins)
	for _, r := range s.records {
		require.NotNil(t, r.Receipt)
	}
}
func TestEffectRecoveryUnknownAndDeniedDoNotBlockLaterOperations(t *testing.T) {
	s := &discoveryEffectStore{records: discoveryRecords()}
	f := &effectFixture{unknown: true}
	denied := s.records[0].Request.OperationKey
	e := effectExecutor(t, s, f, s.records[0].Request, func(_ context.Context, r EffectRequest, a EffectAction) error {
		require.Equal(t, EffectActionReconcile, a)
		if r.OperationKey == denied {
			return errors.New("private denial")
		}
		return nil
	})
	ctx := cortex.WithScope(context.Background(), s.records[0].Request.Scope)
	page, err := e.RecoverPage(ctx, EffectListRequest{Limit: 3})
	require.NoError(t, err)
	require.Len(t, page.Items, 3)
	require.Equal(t, EffectRecoveryUnavailable, page.Items[0].Status)
	require.Equal(t, EffectRecoveryUncertain, page.Items[1].Status)
	require.Equal(t, EffectRecoveryUncertain, page.Items[2].Status)
	require.Equal(t, effectCursor(s.records[2].Request), page.NextAfter)
	require.Equal(t, 2, f.probes)
	require.Zero(t, f.calls)
	for _, item := range page.Items {
		require.Empty(t, item.Outcome)
	}
	for _, r := range s.records {
		require.Nil(t, r.Receipt)
	}
	empty, err := e.RecoverPage(ctx, EffectListRequest{Limit: 3, After: page.NextAfter})
	require.NoError(t, err)
	require.Empty(t, empty.Items)
	require.Empty(t, empty.NextAfter)
}
func TestEffectRecoveryValidatesWholePageBeforeProbes(t *testing.T) {
	for _, mode := range []string{"oversized", "duplicate", "unsorted", "wrong_scope", "wrong_run", "bad_request", "bad_receipt", "before_cursor", "list_error"} {
		t.Run(mode, func(t *testing.T) {
			records := discoveryRecords()
			scope := records[0].Request.Scope
			request := EffectListRequest{Limit: 3, RunID: records[0].Request.RunID}
			switch mode {
			case "oversized":
				request.Limit = 2
			case "duplicate":
				records[2] = records[1]
			case "unsorted":
				records[2], records[1] = records[1], records[2]
			case "wrong_scope":
				records[2].Request.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}}
			case "wrong_run":
				records[2].Request.RunID = id.NewAgentRunID()
			case "bad_request":
				records[2].Request.AttemptID = id.ID{}
			case "bad_receipt":
				receipt := effectApplied(records[0].Request)
				records[2].Receipt = &receipt
			case "before_cursor":
				request.After = effectCursor(records[0].Request)
			}
			s := &discoveryEffectStore{records: records, list: func(context.Context, EffectListRequest) ([]EffectRecord, error) {
				if mode == "list_error" {
					return nil, errors.New("private storage error")
				}
				return records, nil
			}}
			f := &effectFixture{}
			e := effectExecutor(t, s, f, records[0].Request, nil)
			page, err := e.RecoverPage(cortex.WithScope(context.Background(), scope), request)
			require.ErrorIs(t, err, ErrEffectUnavailable)
			require.Empty(t, page.Items)
			require.Zero(t, f.probes)
		})
	}
}
func TestEffectRecoveryAdmissionAndCancellation(t *testing.T) {
	s := &discoveryEffectStore{records: discoveryRecords()}
	f := &effectFixture{}
	e := effectExecutor(t, s, f, s.records[0].Request, nil)
	scoped := cortex.WithScope(context.Background(), s.records[0].Request.Scope)
	for _, req := range []EffectListRequest{{}, {Limit: 257}, {Limit: 1, After: "bad"}, {Limit: 1, RunID: id.NewAgentID()}} {
		_, err := e.RecoverPage(scoped, req)
		require.ErrorIs(t, err, ErrInvalidEffect)
	}
	_, err := e.RecoverPage(context.Background(), EffectListRequest{Limit: 1})
	require.ErrorIs(t, err, cortex.ErrNoScope)
	cancelled, cancel := context.WithCancel(scoped)
	cancel()
	_, err = e.RecoverPage(cancelled, EffectListRequest{Limit: 1})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, s.lists)
	entered, release := make(chan struct{}), make(chan struct{})
	s.list = func(ctx context.Context, _ EffectListRequest) ([]EffectRecord, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		require.LessOrEqual(t, time.Until(deadline), MaxEffectDuration)
		close(entered)
		<-release
		return nil, nil
	}
	done := make(chan error, 1)
	go func() { _, callErr := e.RecoverPage(scoped, EffectListRequest{Limit: 1}); done <- callErr }()
	<-entered
	_, err = e.RecoverPage(scoped, EffectListRequest{Limit: 1})
	require.ErrorIs(t, err, ErrEffectUnavailable)
	close(release)
	require.NoError(t, <-done)
}
func TestEffectRecoveryCancellationKeepsOnlyProcessedCursor(t *testing.T) {
	s := &discoveryEffectStore{records: discoveryRecords()}
	f := &effectFixture{}
	ctx, cancel := context.WithCancel(cortex.WithScope(context.Background(), s.records[0].Request.Scope))
	defer cancel()
	e := effectExecutor(t, s, f, s.records[0].Request, func(_ context.Context, r EffectRequest, _ EffectAction) error {
		if r.OperationKey == s.records[1].Request.OperationKey {
			cancel()
		}
		return nil
	})
	page, err := e.RecoverPage(ctx, EffectListRequest{Limit: 3})
	require.ErrorIs(t, err, context.Canceled)
	require.Len(t, page.Items, 2)
	require.Equal(t, effectCursor(s.records[1].Request), page.NextAfter)
	require.Equal(t, 1, f.probes)
	require.Nil(t, s.records[2].Receipt)
}
func TestEffectRecoveryBoundsDiscoveryTime(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := &discoveryEffectStore{records: discoveryRecords(), list: func(ctx context.Context, _ EffectListRequest) ([]EffectRecord, error) {
			<-ctx.Done()
			return nil, ctx.Err()
		}}
		e := effectExecutor(t, s, &effectFixture{}, s.records[0].Request, nil)
		start := time.Now()
		_, err := e.RecoverPage(cortex.WithScope(context.Background(), s.records[0].Request.Scope), EffectListRequest{Limit: 1})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, MaxEffectDuration, time.Since(start))
	})
}

func TestEffectRecoveryDetachesPageBeforeAuthorizerRuns(t *testing.T) {
	s := &discoveryEffectStore{records: discoveryRecords()}
	listed := append([]EffectRecord(nil), s.records...)
	s.list = func(context.Context, EffectListRequest) ([]EffectRecord, error) { return listed, nil }
	f := &effectFixture{}
	e := effectExecutor(t, s, f, s.records[0].Request, func(_ context.Context, r EffectRequest, _ EffectAction) error {
		if r.OperationKey == s.records[0].Request.OperationKey {
			listed[1].Request.OperationKey = "changed"
		}
		return nil
	})
	page, err := e.RecoverPage(cortex.WithScope(context.Background(), s.records[0].Request.Scope), EffectListRequest{Limit: 3})
	require.NoError(t, err)
	require.Len(t, page.Items, 3)
	require.Equal(t, 3, f.probes)
	for _, item := range page.Items {
		require.Equal(t, EffectRecoveryTerminal, item.Status)
	}
}
func TestEffectRecoveryRevisitsUncertainAfterFullScan(t *testing.T) {
	s := &discoveryEffectStore{records: discoveryRecords()[:1]}
	f := &effectFixture{unknown: true}
	e := effectExecutor(t, s, f, s.records[0].Request, nil)
	ctx := cortex.WithScope(context.Background(), s.records[0].Request.Scope)
	first, err := e.RecoverPage(ctx, EffectListRequest{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, EffectRecoveryUncertain, first.Items[0].Status)
	require.Empty(t, first.NextAfter)
	f.unknown = false
	second, err := e.RecoverPage(ctx, EffectListRequest{Limit: 2, After: first.NextAfter})
	require.NoError(t, err)
	require.Equal(t, EffectRecoveryTerminal, second.Items[0].Status)
	require.Equal(t, 2, f.probes)
	require.Zero(t, f.calls)
}
func TestEffectRecoveryRequiresDiscoveryCapability(t *testing.T) {
	r := effectRequest()
	e := effectExecutor(t, &effectMemoryStore{}, &effectFixture{}, r, nil)
	_, err := e.RecoverPage(cortex.WithScope(context.Background(), r.Scope), EffectListRequest{Limit: 1})
	require.ErrorIs(t, err, ErrEffectUnavailable)
}
