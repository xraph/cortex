package execution

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

type sealedEffectStore struct {
	discoveryEffectStore
	RunCallStore
	seal     *RunCallSeal
	readSeal func(context.Context) (*RunCallSeal, error)
}

func (s *sealedEffectStore) BeginBoundEffect(context.Context, EffectRequest) (bool, error) {
	return false, errors.New("no admission during recovery")
}
func (s *sealedEffectStore) SealRunCalls(context.Context, RunCallSealRequest) (RunCallSeal, error) {
	return RunCallSeal{}, errors.New("no sealing during recovery")
}
func (s *sealedEffectStore) GetRunCallSeal(ctx context.Context, _ id.AgentRunID) (*RunCallSeal, error) {
	if s.readSeal != nil {
		return s.readSeal(ctx)
	}
	return s.seal, nil
}
func newSealedEffectFixture(t *testing.T) (*sealedEffectStore, *effectFixture, RunCallSeal) {
	t.Helper()
	records := discoveryRecords()
	call := effectToolCall(records[0].Request)
	records[0].Request.RunCall = &call
	request := RunCallSealRequest{SchemaVersion: 1, Scope: records[0].Request.Scope, RunID: records[0].Request.RunID, Actor: "recovery", Reason: "worker stopped"}
	_, digest, err := request.Freeze()
	require.NoError(t, err)
	seal := RunCallSeal{Request: request, RequestDigest: digest, At: time.Now().UTC()}
	return &sealedEffectStore{discoveryEffectStore: discoveryEffectStore{records: records}, seal: &seal}, &effectFixture{}, seal
}
func sealedEffectExecutor(t *testing.T, s EffectStore, f EffectAdapter, r EffectRequest, auth func(context.Context, RunCallSeal) error) *EffectExecutor {
	t.Helper()
	e, err := NewEffectExecutor(EffectExecutorConfig{Store: s, Adapters: []EffectRegistration{{Requirement: r.Requirement, Adapter: f}}, Authorize: func(context.Context, EffectRequest, EffectAction) error { return nil }, AuthorizeScan: auth, MaxConcurrent: 1})
	require.NoError(t, err)
	return e
}
func TestSealedEffectRecoveryRetainsOriginalAttempts(t *testing.T) {
	s, f, seal := newSealedEffectFixture(t)
	checks := 0
	e := sealedEffectExecutor(t, s, f, s.records[0].Request, func(_ context.Context, offered RunCallSeal) error {
		checks++
		require.Equal(t, seal, offered)
		offered.Request.Scope.Levels[0].Value = "callback mutation"
		return nil
	})
	ctx := cortex.WithScope(t.Context(), seal.Request.Scope)
	page, err := e.RecoverSealedPage(ctx, EffectRecoveryPageRequest{Seal: seal, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.NotEmpty(t, page.NextAfter)
	require.Equal(t, 2, checks)
	page, err = e.RecoverSealedPage(ctx, EffectRecoveryPageRequest{Seal: seal, Limit: 2, After: page.NextAfter})
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Empty(t, page.NextAfter)
	require.Equal(t, 4, checks)
	require.Equal(t, 3, f.probes)
	require.Zero(t, f.calls)
	require.Zero(t, s.begins)
	for _, r := range s.records {
		require.NotNil(t, r.Receipt)
		require.NoError(t, r.Receipt.Validate(r.Request))
	}
	_, err = e.RecoverSealedPage(ctx, EffectRecoveryPageRequest{Seal: seal, Limit: 3})
	require.NoError(t, err)
	require.Equal(t, 3, f.probes)
}
func TestSealedEffectRecoveryRejectsBeforeDiscovery(t *testing.T) {
	for _, mode := range []string{"no authorizer", "denied", "unsealed", "changed seal", "scope", "missing capability", "cursor", "limit", "invalid seal"} {
		t.Run(mode, func(t *testing.T) {
			s, f, seal := newSealedEffectFixture(t)
			var storage EffectStore = s
			auth := func(context.Context, RunCallSeal) error { return nil }
			ctx := cortex.WithScope(t.Context(), seal.Request.Scope)
			request := EffectRecoveryPageRequest{Seal: seal, Limit: 2}
			switch mode {
			case "no authorizer":
				auth = nil
			case "denied":
				auth = func(context.Context, RunCallSeal) error { return errors.New("denied") }
			case "unsealed":
				s.seal = nil
			case "changed seal":
				other := seal
				other.At = other.At.Add(time.Second)
				s.seal = &other
			case "scope":
				ctx = cortex.WithScope(t.Context(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
			case "missing capability":
				storage = &s.discoveryEffectStore
			case "cursor":
				request.After = "bad"
			case "limit":
				request.Limit = 257
			case "invalid seal":
				request.Seal.RequestDigest = "bad"
			}
			e := sealedEffectExecutor(t, storage, f, s.records[0].Request, auth)
			page, err := e.RecoverSealedPage(ctx, request)
			require.Error(t, err)
			require.Empty(t, page)
			require.Zero(t, s.lists)
			require.Zero(t, f.calls)
			require.Zero(t, f.probes)
		})
	}
}
func TestSealedEffectRecoveryDropsCursorOnAuthorityWithdrawal(t *testing.T) {
	s, f, seal := newSealedEffectFixture(t)
	checks := 0
	e := sealedEffectExecutor(t, s, f, s.records[0].Request, func(context.Context, RunCallSeal) error {
		checks++
		if checks == 2 {
			return errors.New("withdrawn")
		}
		return nil
	})
	ctx := cortex.WithScope(t.Context(), seal.Request.Scope)
	page, err := e.RecoverSealedPage(ctx, EffectRecoveryPageRequest{Seal: seal, Limit: 2})
	require.Error(t, err)
	require.Empty(t, page)
	require.Equal(t, 2, f.probes)
	require.NotNil(t, s.records[0].Receipt)
	require.NotNil(t, s.records[1].Receipt)
	page, err = e.RecoverSealedPage(ctx, EffectRecoveryPageRequest{Seal: seal, Limit: 2})
	require.NoError(t, err)
	require.Len(t, page.Items, 2)
	require.Equal(t, 2, f.probes)
}

func TestSealedEffectRecoveryRejectsChangedPostScanSeal(t *testing.T) {
	s, f, seal := newSealedEffectFixture(t)
	reads := 0
	s.readSeal = func(context.Context) (*RunCallSeal, error) {
		reads++
		retained := seal
		if reads == 2 {
			retained.At = retained.At.Add(time.Second)
		}
		return &retained, nil
	}
	e := sealedEffectExecutor(t, s, f, s.records[0].Request, func(context.Context, RunCallSeal) error { return nil })
	page, err := e.RecoverSealedPage(cortex.WithScope(t.Context(), seal.Request.Scope), EffectRecoveryPageRequest{Seal: seal, Limit: 2})
	require.ErrorIs(t, err, ErrRunCallConflict)
	require.Empty(t, page)
	require.Equal(t, 2, f.probes)
}
func TestSealedEffectRecoveryRetainsUnknownAndRejectsUnavailable(t *testing.T) {
	s, f, seal := newSealedEffectFixture(t)
	f.unknown = true
	e := sealedEffectExecutor(t, s, f, s.records[0].Request, func(context.Context, RunCallSeal) error { return nil })
	ctx := cortex.WithScope(t.Context(), seal.Request.Scope)
	page, err := e.RecoverSealedPage(ctx, EffectRecoveryPageRequest{Seal: seal, Limit: 2})
	require.NoError(t, err)
	require.NotEmpty(t, page.NextAfter)
	for _, item := range page.Items {
		require.Equal(t, EffectRecoveryUncertain, item.Status)
	}
	f.unknown = false
	e.authorize = func(context.Context, EffectRequest, EffectAction) error { return errors.New("operation denied") }
	page, err = e.RecoverSealedPage(ctx, EffectRecoveryPageRequest{Seal: seal, Limit: 2})
	require.ErrorIs(t, err, ErrEffectUnavailable)
	require.Empty(t, page)
	require.Equal(t, 2, f.probes)
}
func TestSealedEffectRecoveryBoundsAllScanPorts(t *testing.T) {
	for _, mode := range []string{"queue", "authorize", "seal", "discovery"} {
		t.Run(mode, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s, f, seal := newSealedEffectFixture(t)
				auth := func(context.Context, RunCallSeal) error { return nil }
				switch mode {
				case "authorize":
					auth = func(ctx context.Context, _ RunCallSeal) error { <-ctx.Done(); return ctx.Err() }
				case "seal":
					s.readSeal = func(ctx context.Context) (*RunCallSeal, error) { <-ctx.Done(); return nil, ctx.Err() }
				case "discovery":
					s.list = func(ctx context.Context, _ EffectListRequest) ([]EffectRecord, error) {
						<-ctx.Done()
						return nil, ctx.Err()
					}
				}
				e := sealedEffectExecutor(t, s, f, s.records[0].Request, auth)
				if mode == "queue" {
					e.scanSlots <- struct{}{}
				}
				start := time.Now()
				page, err := e.RecoverSealedPage(cortex.WithScope(t.Context(), seal.Request.Scope), EffectRecoveryPageRequest{Seal: seal, Limit: 2})
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.Empty(t, page)
				require.Zero(t, f.probes)
				require.Equal(t, MaxEffectDuration, time.Since(start))
			})
		})
	}
}
func TestSealedEffectRecoveryValidatesAllRowsBeforeProbe(t *testing.T) {
	s, f, seal := newSealedEffectFixture(t)
	s.records[1].Request.AttemptID = id.ID{}
	e := sealedEffectExecutor(t, s, f, s.records[0].Request, func(context.Context, RunCallSeal) error { return nil })
	page, err := e.RecoverSealedPage(cortex.WithScope(t.Context(), seal.Request.Scope), EffectRecoveryPageRequest{Seal: seal, Limit: 2})
	require.Error(t, err)
	require.Empty(t, page)
	require.Zero(t, f.probes)
}
