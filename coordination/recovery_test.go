package coordination_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

type expiryStore struct {
	rows      []coordination.Claim
	calls     int
	listCalls int
	listErr   error
	expire    func(context.Context, id.ID, id.ID, int64) (coordination.Claim, error)
}

func (s *expiryStore) ListTaskClaims(_ context.Context, _ coordination.ClaimListRequest) ([]coordination.Claim, error) {
	s.listCalls++
	return s.rows, s.listErr
}
func (s *expiryStore) ExpireTaskClaim(ctx context.Context, plan, task id.ID, revision int64) (coordination.Claim, error) {
	s.calls++
	return s.expire(ctx, plan, task, revision)
}
func expiryFixture(t *testing.T) (context.Context, coordination.Claim, time.Time) {
	t.Helper()
	h, p, _, now := handoffFixture(t)
	claim, err := coordination.AcquireClaim(nil, coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}, p, now)
	require.NoError(t, err)
	return cortex.WithScope(context.Background(), claim.Scope), claim, now
}
func TestClaimExpirySweepRetainsDispatchUncertainty(t *testing.T) {
	for _, dispatched := range []bool{false, true} {
		t.Run(map[bool]string{false: "unused", true: "dispatched"}[dispatched], func(t *testing.T) {
			ctx, claim, now := expiryFixture(t)
			var err error
			if dispatched {
				claim, err = coordination.BeginDispatch(claim, claim.Token(), id.NewAgentRunID(), now)
				require.NoError(t, err)
			}
			s := &expiryStore{rows: []coordination.Claim{claim}}
			s.expire = func(got context.Context, plan, task id.ID, revision int64) (coordination.Claim, error) {
				require.Equal(t, claim.Scope, cortex.ScopeFromContext(got))
				require.Equal(t, claim.PlanID, plan)
				require.Equal(t, claim.TaskID, task)
				require.Equal(t, claim.Revision, revision)
				next, expireErr := coordination.ExpireClaim(claim, revision, claim.ExpiresAt)
				s.rows = []coordination.Claim{next}
				return next, expireErr
			}
			sweeper, err := coordination.NewClaimSweeper(s, schedulerClock{at: claim.ExpiresAt})
			require.NoError(t, err)
			request := coordination.ClaimListRequest{PlanID: claim.PlanID, Limit: 1}
			page, err := sweeper.Sweep(ctx, request)
			require.NoError(t, err)
			require.Equal(t, 1, page.Scanned)
			require.Equal(t, claim.TaskID, page.NextTaskID)
			if dispatched {
				require.Equal(t, 1, page.Recovering)
				require.Zero(t, page.Released)
				require.Equal(t, claim.RunID, s.rows[0].RunID)
			} else {
				require.Equal(t, 1, page.Released)
				require.Zero(t, page.Recovering)
			}
			page, err = sweeper.Sweep(ctx, request)
			require.NoError(t, err)
			require.Equal(t, 1, s.calls, "terminal observations must not expire twice")
			require.Zero(t, page.Released+page.Recovering)
		})
	}
}
func TestClaimExpirySweepValidatesWholePageBeforeWrites(t *testing.T) {
	for _, mode := range []string{"scope", "plan", "digest", "duplicate", "cursor", "oversized", "future"} {
		t.Run(mode, func(t *testing.T) {
			ctx, claim, _ := expiryFixture(t)
			s := &expiryStore{rows: []coordination.Claim{claim}}
			request := coordination.ClaimListRequest{PlanID: claim.PlanID, Limit: 2}
			at := claim.ExpiresAt
			switch mode {
			case "scope":
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
			case "plan":
				request.PlanID = id.New(planning.PrefixPlan)
			case "digest":
				bad := claim
				bad.Digest = "bad"
				s.rows = append(s.rows, bad)
			case "duplicate":
				s.rows = append(s.rows, claim)
			case "cursor":
				request.AfterTaskID = claim.TaskID
			case "oversized":
				request.Limit = 1
				s.rows = append(s.rows, claim)
			case "future":
				at = claim.At.Add(-time.Second)
			}
			sweeper, err := coordination.NewClaimSweeper(s, schedulerClock{at: at})
			require.NoError(t, err)
			page, err := sweeper.Sweep(ctx, request)
			require.ErrorIs(t, err, coordination.ErrInvalidClaim)
			require.Zero(t, page)
			require.Zero(t, s.calls)
		})
	}
}
func TestClaimExpirySweepHandlesConflictsAndUncertainWrites(t *testing.T) {
	for _, mode := range []string{"conflict", "lost_ack", "bad_receipt", "cancel", "live"} {
		t.Run(mode, func(t *testing.T) {
			ctx, claim, now := expiryFixture(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			injected := errors.New("storage acknowledgement lost")
			s := &expiryStore{rows: []coordination.Claim{claim}}
			s.expire = func(context.Context, id.ID, id.ID, int64) (coordination.Claim, error) {
				switch mode {
				case "conflict":
					return coordination.Claim{}, coordination.ErrClaimConflict
				case "lost_ack":
					return coordination.Claim{}, injected
				case "bad_receipt":
					return claim, nil
				default:
					t.Fatal("unexpected write")
					return coordination.Claim{}, nil
				}
			}
			at := claim.ExpiresAt
			if mode == "live" {
				at = now
			}
			if mode == "cancel" {
				cancel()
			}
			sweeper, err := coordination.NewClaimSweeper(s, schedulerClock{at: at})
			require.NoError(t, err)
			page, err := sweeper.Sweep(ctx, coordination.ClaimListRequest{PlanID: claim.PlanID, Limit: 2})
			switch mode {
			case "conflict":
				require.NoError(t, err)
				require.Equal(t, 1, page.Conflicts)
			case "lost_ack":
				require.ErrorIs(t, err, injected)
				require.Zero(t, page)
			case "bad_receipt":
				require.ErrorIs(t, err, coordination.ErrInvalidClaim)
				require.Zero(t, page)
			case "cancel":
				require.ErrorIs(t, err, context.Canceled)
				require.Zero(t, s.listCalls)
			case "live":
				require.NoError(t, err)
				require.Zero(t, s.calls)
			}
		})
	}
}
func TestClaimExpirySweepRejectsMissingScopeAndBounds(t *testing.T) {
	ctx, claim, _ := expiryFixture(t)
	s := &expiryStore{}
	sweeper, err := coordination.NewClaimSweeper(s, schedulerClock{at: claim.ExpiresAt})
	require.NoError(t, err)
	_, err = sweeper.Sweep(context.Background(), coordination.ClaimListRequest{PlanID: claim.PlanID, Limit: 1})
	require.ErrorIs(t, err, cortex.ErrNoScope)
	for _, req := range []coordination.ClaimListRequest{{PlanID: claim.PlanID}, {PlanID: claim.PlanID, Limit: 257}, {Limit: 1}, {PlanID: claim.PlanID, Limit: 1, AfterTaskID: claim.PlanID}} {
		_, err = sweeper.Sweep(ctx, req)
		require.ErrorIs(t, err, coordination.ErrInvalidClaim)
	}
	require.Zero(t, s.listCalls)
	var missing *expiryStore
	_, err = coordination.NewClaimSweeper(missing, schedulerClock{at: claim.ExpiresAt})
	require.ErrorIs(t, err, coordination.ErrInvalidClaim)
	_, err = coordination.NewClaimSweeper(s, nil)
	require.ErrorIs(t, err, coordination.ErrInvalidClaim)
}

func TestClaimExpirySweepCancelsAfterCommittedWrite(t *testing.T) {
	h, p, _, now := handoffFixture(t)
	other := p.Tasks[0]
	other.ID = id.New(planning.PrefixTask)
	other.Output.Name = "second-report"
	p.Tasks = append(p.Tasks, other)
	p, err := planning.Seal(p, planning.Limits{})
	require.NoError(t, err)
	rows := make([]coordination.Claim, 0, 2)
	for _, task := range p.Tasks {
		claim, claimErr := coordination.AcquireClaim(nil, coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: task.ID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}, p, now)
		require.NoError(t, claimErr)
		rows = append(rows, claim)
	}
	if rows[0].TaskID.String() > rows[1].TaskID.String() {
		rows[0], rows[1] = rows[1], rows[0]
	}
	ctx, cancel := context.WithCancel(cortex.WithScope(context.Background(), p.Scope))
	defer cancel()
	s := &expiryStore{rows: rows}
	s.expire = func(context.Context, id.ID, id.ID, int64) (coordination.Claim, error) {
		next, expireErr := coordination.ExpireClaim(rows[0], rows[0].Revision, rows[0].ExpiresAt)
		cancel()
		return next, expireErr
	}
	sweeper, err := coordination.NewClaimSweeper(s, schedulerClock{at: now.Add(time.Minute)})
	require.NoError(t, err)
	page, err := sweeper.Sweep(ctx, coordination.ClaimListRequest{PlanID: p.ID, Limit: 2})
	require.ErrorIs(t, err, context.Canceled)
	require.Zero(t, page)
	require.Equal(t, 1, s.calls)
}
