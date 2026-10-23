package coordination_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func readyAdmissionFixture(t *testing.T) (coordination.ReadyTaskClaimRequest, coordination.ReadyTaskClaim) {
	t.Helper()
	h, p, _, now := handoffFixture(t)
	cr := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}
	claim, err := coordination.AcquireClaim(nil, cr, p, now)
	require.NoError(t, err)
	reservation := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: h.TaskID, OwnerRef: cr.ID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{FanOut: 1}, Deadline: p.Budget.Deadline}
	_, rd, err := reservation.Freeze()
	require.NoError(t, err)
	req := coordination.ReadyTaskClaimRequest{SchemaVersion: 1, Acquisition: coordination.BudgetedClaimRequest{Claim: cr, Reservation: reservation}}
	req, digest, err := req.Freeze()
	require.NoError(t, err)
	result := coordination.ReadyTaskClaim{RequestDigest: digest, Acquisition: coordination.BudgetedClaim{Claim: claim, Reservation: execution.Reservation{Request: reservation, RequestDigest: rd, CreatedAt: now}}, Outbox: coordination.TaskClaimOutbox{EventID: id.New(coordination.PrefixTaskClaimedEvent), Type: "task_claimed", ClaimID: cr.ID, PlanID: p.ID, TaskID: h.TaskID}, Fresh: true}
	return req, result
}

type taskClaimFenceFunc func(context.Context, coordination.ReadyTaskClaimRequest, func(coordination.ReadyTaskClaimRequest) error) error

func (f taskClaimFenceFunc) WithTaskClaimFence(ctx context.Context, r coordination.ReadyTaskClaimRequest, next func(coordination.ReadyTaskClaimRequest) error) error {
	return f(ctx, r, next)
}

type taskClaimAuthorizeFunc func(context.Context, coordination.ReadyTaskClaimRequest) error

func (f taskClaimAuthorizeFunc) AuthorizeTaskClaim(ctx context.Context, r coordination.ReadyTaskClaimRequest) error {
	return f(ctx, r)
}

type readyClaimStorage struct {
	coordination.ReadyTaskClaimStore
	claim func(context.Context, coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error)
}

func (s readyClaimStorage) ClaimReadyTask(ctx context.Context, r coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
	return s.claim(ctx, r)
}

func TestTaskClaimAdmitterHoldsFenceAndDetachesValues(t *testing.T) {
	req, receipt := readyAdmissionFixture(t)
	ctx := cortex.WithScope(context.Background(), req.Acquisition.Reservation.Scope)
	var steps []string
	held := false
	fence := taskClaimFenceFunc(func(_ context.Context, current coordination.ReadyTaskClaimRequest, next func(coordination.ReadyTaskClaimRequest) error) error {
		held = true
		steps = append(steps, "acquire")
		defer func() { held = false; steps = append(steps, "release") }()
		return next(current)
	})
	authorizer := taskClaimAuthorizeFunc(func(_ context.Context, current coordination.ReadyTaskClaimRequest) error {
		require.True(t, held)
		steps = append(steps, "authorize")
		current.Acquisition.Reservation.Scope.Levels[0].Value = "mutated"
		return nil
	})
	storage := readyClaimStorage{claim: func(_ context.Context, current coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
		require.True(t, held)
		require.Equal(t, req, current)
		steps = append(steps, "store")
		return receipt, nil
	}}
	admitter, err := coordination.NewTaskClaimAdmitter(storage, fence, authorizer)
	require.NoError(t, err)
	got, err := admitter.Admit(ctx, req)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	require.Equal(t, []string{"acquire", "authorize", "store", "release"}, steps)
	receipt.Acquisition.Claim.Scope.Levels[0].Value = "storage mutation"
	require.NoError(t, got.Validate(req))
}

func TestTaskClaimAdmitterRejectsChangedAuthorityAndCallbacks(t *testing.T) {
	denied := errors.New("revoked")
	for _, mode := range []string{"denied", "changed_worker", "changed_grant", "changed_budget", "scope", "cancelled", "missing_callback", "duplicate_callback", "lost_ack", "bad_receipt", "unrelated_receipt", "forged_request_digest", "cancel_in_authorizer"} {
		t.Run(mode, func(t *testing.T) {
			req, receipt := readyAdmissionFixture(t)
			ctx := cortex.WithScope(context.Background(), req.Acquisition.Reservation.Scope)
			if mode == "scope" {
				ctx = context.Background()
			}
			var cancelDuring context.CancelFunc
			if mode == "cancel_in_authorizer" {
				ctx, cancelDuring = context.WithCancel(ctx)
				defer cancelDuring()
			}
			if mode == "cancelled" {
				bounded, cancel := context.WithCancel(ctx)
				cancel()
				ctx = bounded
			}
			calls := 0
			storage := readyClaimStorage{claim: func(context.Context, coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
				calls++
				if mode == "bad_receipt" {
					receipt.Outbox.Type = "unverified"
				}
				if mode == "unrelated_receipt" || mode == "forged_request_digest" {
					originalDigest := receipt.RequestDigest
					_, receipt = readyAdmissionFixture(t)
					if mode == "forged_request_digest" {
						receipt.RequestDigest = originalDigest
					}
				}
				return receipt, nil
			}}
			fence := taskClaimFenceFunc(func(_ context.Context, current coordination.ReadyTaskClaimRequest, next func(coordination.ReadyTaskClaimRequest) error) error {
				switch mode {
				case "changed_worker":
					current.Acquisition.Claim.Worker.Principal = "other"
				case "changed_grant":
					current.Acquisition.Claim.GrantRef = "revoked"
				case "changed_budget":
					current.Acquisition.Reservation.Allocation.Tokens++
				case "missing_callback":
					return nil
				case "duplicate_callback":
					_ = next(current)
					_ = next(current)
					return nil
				case "lost_ack":
					_ = next(current)
					return denied
				}
				return next(current)
			})
			authorizer := taskClaimAuthorizeFunc(func(context.Context, coordination.ReadyTaskClaimRequest) error {
				if mode == "denied" {
					return denied
				}
				if mode == "cancel_in_authorizer" {
					cancelDuring()
				}
				return nil
			})
			admitter, err := coordination.NewTaskClaimAdmitter(storage, fence, authorizer)
			require.NoError(t, err)
			got, err := admitter.Admit(ctx, req)
			require.Error(t, err)
			require.Zero(t, got)
			if mode == "duplicate_callback" || mode == "lost_ack" || mode == "bad_receipt" || mode == "unrelated_receipt" || mode == "forged_request_digest" {
				require.Equal(t, 1, calls)
			} else {
				require.Zero(t, calls)
			}
		})
	}
}

func TestTaskClaimAdmitterChecksWholeChainAndReauthorizesRetry(t *testing.T) {
	req, receipt := readyAdmissionFixture(t)
	accepted := taskAcceptanceFixture(t)
	req.Dependencies = []coordination.AcceptedDependency{{AcceptanceID: accepted.Request.ID, Digest: accepted.Digest, Current: accepted.Request.Current}}
	req.Dependencies[0].Current.Scope = req.Acquisition.Reservation.Scope
	var err error
	req, receipt.RequestDigest, err = req.Freeze()
	require.NoError(t, err)
	receipt.Fresh = false
	ctx := cortex.WithScope(context.Background(), req.Acquisition.Reservation.Scope)
	allowed, stale := true, false
	calls, authorizations := 0, 0
	fence := taskClaimFenceFunc(func(_ context.Context, current coordination.ReadyTaskClaimRequest, next func(coordination.ReadyTaskClaimRequest) error) error {
		if stale {
			current.Dependencies[0].Current.Context.PolicyRevision = "changed"
		}
		return next(current)
	})
	authorizer := taskClaimAuthorizeFunc(func(_ context.Context, current coordination.ReadyTaskClaimRequest) error {
		authorizations++
		require.Equal(t, req.Dependencies, current.Dependencies)
		if !allowed {
			return errors.New("revoked")
		}
		return nil
	})
	storage := readyClaimStorage{claim: func(context.Context, coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
		calls++
		return receipt, nil
	}}
	admitter, err := coordination.NewTaskClaimAdmitter(storage, fence, authorizer)
	require.NoError(t, err)
	got, err := admitter.Admit(ctx, req)
	require.NoError(t, err)
	require.False(t, got.Fresh)
	allowed = false
	_, err = admitter.Admit(ctx, req)
	require.Error(t, err)
	require.Equal(t, 2, authorizations)
	require.Equal(t, 1, calls)
	allowed, stale = true, true
	_, err = admitter.Admit(ctx, req)
	require.ErrorIs(t, err, coordination.ErrClaimConflict)
	require.Equal(t, 2, authorizations)
	require.Equal(t, 1, calls)
}

func TestTaskClaimAdmitterRejectsMissingPorts(t *testing.T) {
	fence := taskClaimFenceFunc(func(_ context.Context, current coordination.ReadyTaskClaimRequest, next func(coordination.ReadyTaskClaimRequest) error) error {
		return next(current)
	})
	authorizer := taskClaimAuthorizeFunc(func(context.Context, coordination.ReadyTaskClaimRequest) error { return nil })
	storage := readyClaimStorage{}
	for _, mode := range []string{"storage", "fence", "authorizer"} {
		var s coordination.ReadyTaskClaimStore = storage
		var f coordination.TaskClaimFence = fence
		var a coordination.TaskClaimAuthorizer = authorizer
		switch mode {
		case "storage":
			s = (*readyClaimStorage)(nil)
		case "fence":
			f = taskClaimFenceFunc(nil)
		case "authorizer":
			a = taskClaimAuthorizeFunc(nil)
		}
		_, err := coordination.NewTaskClaimAdmitter(s, f, a)
		require.Error(t, err)
	}
}

func TestTaskClaimAdmitterRejectsEscapedAndUnfinishedCallbacks(t *testing.T) {
	for _, async := range []bool{false, true} {
		req, receipt := readyAdmissionFixture(t)
		ctx := cortex.WithScope(context.Background(), req.Acquisition.Reservation.Scope)
		var escaped func(coordination.ReadyTaskClaimRequest) error
		started := make(chan struct{})
		finished := make(chan error, 1)
		storage := readyClaimStorage{claim: func(context.Context, coordination.ReadyTaskClaimRequest) (coordination.ReadyTaskClaim, error) {
			t.Error("escaped callback reached storage")
			return receipt, nil
		}}
		fence := taskClaimFenceFunc(func(_ context.Context, current coordination.ReadyTaskClaimRequest, next func(coordination.ReadyTaskClaimRequest) error) error {
			escaped = next
			if async {
				go func() { finished <- next(current) }()
				<-started
			}
			return nil
		})
		authorizer := taskClaimAuthorizeFunc(func(ctx context.Context, _ coordination.ReadyTaskClaimRequest) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		})
		admitter, err := coordination.NewTaskClaimAdmitter(storage, fence, authorizer)
		require.NoError(t, err)
		got, err := admitter.Admit(ctx, req)
		require.Error(t, err)
		require.Zero(t, got)
		if async {
			require.Error(t, <-finished)
		} else {
			require.Error(t, escaped(req))
		}
	}
}

func TestReadyTaskClaimReceiptRejectsCrossBindings(t *testing.T) {
	for _, mode := range []string{"valid", "outbox", "request", "claim", "reservation", "scope", "lease", "nested_fresh", "larger_allocation"} {
		t.Run(mode, func(t *testing.T) {
			req, receipt := readyAdmissionFixture(t)
			switch mode {
			case "outbox":
				receipt.Outbox.ClaimID = id.New(coordination.PrefixClaim)
			case "request":
				req.Acquisition.Claim.GrantRef = "other"
			case "claim":
				receipt.Acquisition.Claim.Worker.Principal = "other"
			case "reservation":
				receipt.Acquisition.Reservation.Request.ID = id.New(execution.PrefixReservation)
			case "scope":
				receipt.Acquisition.Reservation.Request.Scope.Levels[0].Value = "other"
			case "lease":
				req.Acquisition.Claim.Duration = time.Second
				_, receipt.RequestDigest, _ = req.Freeze()
			case "nested_fresh":
				receipt.Acquisition.Fresh = true
			case "larger_allocation":
				receipt.Acquisition.Reservation.Request.Allocation.Tokens++
				_, receipt.Acquisition.Reservation.RequestDigest, _ = receipt.Acquisition.Reservation.Request.Freeze()
			}
			err := receipt.Validate(req)
			if mode == "valid" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
