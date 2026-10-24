package execution_test

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

type recoveryScanMemory struct {
	execution.RunCallSealStore
	seal          *execution.RunCallSeal
	rows          []execution.RunCallRecord
	receipts      map[string]execution.ProviderCallReceipt
	lists, writes int
	list          func(context.Context, execution.RunCallListRequest) ([]execution.RunCallRecord, error)
}

func (s *recoveryScanMemory) GetRunCallSeal(context.Context, id.AgentRunID) (*execution.RunCallSeal, error) {
	return s.seal, nil
}
func (s *recoveryScanMemory) ListRunCalls(ctx context.Context, r execution.RunCallListRequest) ([]execution.RunCallRecord, error) {
	s.lists++
	if s.list != nil {
		return s.list(ctx, r)
	}
	var rows []execution.RunCallRecord
	for _, row := range s.rows {
		if row.Request.Key() > r.After && len(rows) < r.Limit {
			rows = append(rows, row)
		}
	}
	return rows, nil
}
func (s *recoveryScanMemory) GetRunCall(_ context.Context, _ id.AgentRunID, operation string) (execution.RunCallRecord, error) {
	for _, row := range s.rows {
		if row.Request.OperationDigest == operation {
			return row, nil
		}
	}
	return execution.RunCallRecord{}, execution.ErrRunCallNotFound
}
func (s *recoveryScanMemory) GetProviderCallReceipt(_ context.Context, r execution.RunCallRequest) (*execution.ProviderCallReceipt, error) {
	receipt, ok := s.receipts[r.Key()]
	if !ok {
		return nil, nil
	}
	return &receipt, nil
}
func (s *recoveryScanMemory) PutProviderCallReceipt(_ context.Context, r execution.RunCallRequest, receipt execution.ProviderCallReceipt) error {
	s.writes++
	s.receipts[r.Key()] = receipt
	return nil
}
func recoveryScanFixture(t *testing.T) (context.Context, *recoveryScanMemory, execution.ProviderRecoveryConfig, execution.ProviderRecoveryPageRequest, *int) {
	t.Helper()
	r := runCallRequest()
	sr := execution.RunCallSealRequest{SchemaVersion: 1, Scope: r.Scope, RunID: r.RunID, Actor: "recovery", Reason: "interrupted worker"}
	_, digest, err := sr.Freeze()
	require.NoError(t, err)
	seal := execution.RunCallSeal{Request: sr, RequestDigest: digest, At: time.Now().UTC()}
	s := &recoveryScanMemory{seal: &seal, receipts: map[string]execution.ProviderCallReceipt{}}
	for _, name := range []string{"one", "two", "three"} {
		call := r
		call.ID = id.New(execution.PrefixRunCall)
		call.OperationDigest = evidence.Digest([]byte(name))
		s.rows = append(s.rows, execution.RunCallRecord{Request: call, StartedAt: seal.At.Add(-time.Second)})
	}
	sort.Slice(s.rows, func(i, j int) bool { return s.rows[i].Request.Key() < s.rows[j].Request.Key() })
	s.rows[1].Request.Kind = execution.RunCallTool
	s.rows[2].Request.Kind = execution.RunCallStream
	probes := new(int)
	config := execution.ProviderRecoveryConfig{
		Store: s,
		Adapter: providerProbe(func(_ context.Context, call execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
			*probes++
			if call.Kind == execution.RunCallStream {
				return execution.ProviderCallReceipt{}, execution.ErrProviderCallUncertain
			}
			return providerReceipt(t, call), nil
		}),
		Authorize:     func(context.Context, execution.RunCallRequest) error { return nil },
		Authenticate:  func(context.Context, execution.RunCallRequest, execution.ProviderCallReceipt) error { return nil },
		Fence:         providerFence(func(_ context.Context, _ execution.RunCallRequest, next func() error) error { return next() }),
		AuthorizeScan: func(context.Context, execution.RunCallSeal) error { return nil },
	}
	return cortex.WithScope(t.Context(), r.Scope), s, config, execution.ProviderRecoveryPageRequest{Seal: seal, Limit: 2}, probes
}

func TestProviderRecoveryScanPagesPreserveUncertainty(t *testing.T) {
	ctx, s, config, request, probes := recoveryScanFixture(t)
	service, err := execution.NewProviderReconciler(config)
	require.NoError(t, err)
	page, err := service.ReconcilePage(ctx, request)
	require.NoError(t, err)
	require.Equal(t, execution.ProviderRecoveryPage{Scanned: 2, Terminal: 1, ToolCalls: 1, NextAfter: s.rows[1].Request.Key()}, page)
	request.After = page.NextAfter
	page, err = service.ReconcilePage(ctx, request)
	require.NoError(t, err)
	require.Equal(t, execution.ProviderRecoveryPage{Scanned: 1, Unresolved: 1}, page)
	require.Equal(t, 2, *probes)
	require.Equal(t, 1, s.writes)
	// A new pass reauthenticates retained proof and revisits unresolved calls.
	request.After = ""
	request.Limit = 256
	page, err = service.ReconcilePage(ctx, request)
	require.NoError(t, err)
	require.Equal(t, execution.ProviderRecoveryPage{Scanned: 3, Terminal: 1, ToolCalls: 1, Unresolved: 1}, page)
	require.Equal(t, 3, *probes)
	require.Equal(t, 1, s.writes)
}

func TestProviderRecoveryScanValidatesWholePageBeforeQueries(t *testing.T) {
	for _, name := range []string{"oversize", "duplicate", "unordered", "scope", "run", "invalid", "cursor"} {
		t.Run(name, func(t *testing.T) {
			ctx, s, config, request, probes := recoveryScanFixture(t)
			rows := append([]execution.RunCallRecord(nil), s.rows...)
			request.Limit = 3
			switch name {
			case "oversize":
				request.Limit = 2
			case "duplicate":
				rows[2] = rows[1]
			case "unordered":
				rows[1], rows[2] = rows[2], rows[1]
			case "scope":
				rows[2].Request.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}}
			case "run":
				rows[2].Request.RunID = id.NewAgentRunID()
			case "invalid":
				rows[2].StartedAt = time.Time{}
			case "cursor":
				request.After = rows[0].Request.Key()
			}
			s.list = func(context.Context, execution.RunCallListRequest) ([]execution.RunCallRecord, error) {
				return rows, nil
			}
			service, err := execution.NewProviderReconciler(config)
			require.NoError(t, err)
			page, err := service.ReconcilePage(ctx, request)
			require.Error(t, err)
			require.Empty(t, page)
			require.Zero(t, *probes)
			require.Zero(t, s.writes)
		})
	}
}

func TestProviderRecoveryScanRequiresCurrentSealAndAuthority(t *testing.T) {
	for _, name := range []string{"unsealed", "changed seal", "changed time", "denied", "revoked", "no authorizer", "wrong scope", "bad cursor", "bad limit", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			ctx, s, config, request, probes := recoveryScanFixture(t)
			denied := errors.New("denied")
			switch name {
			case "unsealed":
				s.seal = nil
			case "changed seal":
				changed := *s.seal
				changed.Request.Reason = "another reason"
				_, changed.RequestDigest, _ = changed.Request.Freeze()
				s.seal = &changed
			case "changed time":
				changed := *s.seal
				changed.At = changed.At.Add(time.Second)
				s.seal = &changed
			case "denied":
				config.AuthorizeScan = func(context.Context, execution.RunCallSeal) error { return denied }
			case "revoked":
				calls := 0
				config.AuthorizeScan = func(context.Context, execution.RunCallSeal) error {
					calls++
					if calls > 1 {
						return denied
					}
					return nil
				}
			case "no authorizer":
				config.AuthorizeScan = nil
			case "wrong scope":
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
			case "bad cursor":
				request.After = "bad"
			case "bad limit":
				request.Limit = 257
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			service, err := execution.NewProviderReconciler(config)
			require.NoError(t, err)
			page, err := service.ReconcilePage(ctx, request)
			require.Error(t, err)
			require.Empty(t, page)
			if name == "revoked" {
				require.Equal(t, 1, s.writes)
			} else {
				require.Zero(t, *probes)
			}
		})
	}
}

func TestProviderRecoveryScanReturnsNoCursorOnPartialFailure(t *testing.T) {
	ctx, s, config, request, _ := recoveryScanFixture(t)
	request.Limit = 3
	config.Authenticate = func(_ context.Context, call execution.RunCallRequest, _ execution.ProviderCallReceipt) error {
		if call.Kind == execution.RunCallStream {
			return errors.New("invalid proof")
		}
		return nil
	}
	config.Adapter = providerProbe(func(_ context.Context, call execution.RunCallRequest) (execution.ProviderCallReceipt, error) {
		return providerReceipt(t, call), nil
	})
	service, err := execution.NewProviderReconciler(config)
	require.NoError(t, err)
	page, err := service.ReconcilePage(ctx, request)
	require.ErrorContains(t, err, "invalid proof")
	require.Empty(t, page)
	require.Equal(t, 1, s.writes, "earlier authenticated proof remains durable for retry")
}

func TestProviderRecoveryScanBoundsQueueAndPageTime(t *testing.T) {
	ctx, s, config, request, _ := recoveryScanFixture(t)
	config.MaxConcurrent = 1
	config.Timeout = time.Second
	entered, release := make(chan struct{}), make(chan struct{})
	s.list = func(ctx context.Context, _ execution.RunCallListRequest) ([]execution.RunCallRecord, error) {
		close(entered)
		select {
		case <-release:
			return nil, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	service, err := execution.NewProviderReconciler(config)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() { _, scanErr := service.ReconcilePage(ctx, request); done <- scanErr }()
	<-entered
	queued, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	page, err := service.ReconcilePage(queued, request)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, page)
	close(release)
	require.NoError(t, <-done)
	require.Equal(t, 1, s.lists, "queued page cannot reach storage")
	config.Timeout = 10 * time.Millisecond
	s.list = func(ctx context.Context, _ execution.RunCallListRequest) ([]execution.RunCallRecord, error) {
		<-ctx.Done()
		return nil, nil
	}
	service, err = execution.NewProviderReconciler(config)
	require.NoError(t, err)
	page, err = service.ReconcilePage(ctx, request)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Empty(t, page)
}

func TestProviderRecoveryScanDetachesHostInputsAndChecksSealAgain(t *testing.T) {
	for _, changed := range []bool{false, true} {
		ctx, s, config, request, _ := recoveryScanFixture(t)
		config.AuthorizeScan = func(_ context.Context, seal execution.RunCallSeal) error {
			seal.Request.Scope.Levels[0].Value = "host mutation"
			return nil
		}
		config.Authenticate = func(context.Context, execution.RunCallRequest, execution.ProviderCallReceipt) error {
			if changed {
				s.seal = nil
			}
			return nil
		}
		service, err := execution.NewProviderReconciler(config)
		require.NoError(t, err)
		page, err := service.ReconcilePage(ctx, request)
		if changed {
			require.ErrorIs(t, err, execution.ErrRunCallConflict)
			require.Empty(t, page)
		} else {
			require.NoError(t, err)
			require.Equal(t, 1, page.Terminal)
		}
		require.Equal(t, "one", request.Seal.Request.Scope.Levels[0].Value)
	}
}

func TestProviderRecoveryScanEmptyJournalAndUnavailableStore(t *testing.T) {
	ctx, s, config, request, probes := recoveryScanFixture(t)
	s.rows = nil
	service, err := execution.NewProviderReconciler(config)
	require.NoError(t, err)
	page, err := service.ReconcilePage(ctx, request)
	require.NoError(t, err)
	require.Empty(t, page)
	require.Zero(t, *probes)
	config.Store = &providerReceiptMemory{}
	service, err = execution.NewProviderReconciler(config)
	require.NoError(t, err)
	page, err = service.ReconcilePage(ctx, request)
	require.ErrorIs(t, err, execution.ErrRunCallUnavailable)
	require.Empty(t, page)
}
