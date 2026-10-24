package coordination_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

func renewalFixture(t *testing.T) (coordination.TaskRenewalRequest, coordination.TaskRenewal) {
	t.Helper()
	_, admission := readyAdmissionFixture(t)
	previous := admission.Acquisition.Claim
	claim, err := coordination.BeginDispatch(previous, previous.Token(), id.NewAgentRunID(), previous.At)
	require.NoError(t, err)
	req := coordination.TaskRenewalRequest{Actor: claim.Worker.Principal, Reason: "retain active ownership", SchemaVersion: 1, Claim: claim, ParentRunID: id.NewAgentRunID(), ParentRunRevision: 1, RunRevision: 1, AssignmentID: id.New(coordination.PrefixAssignment), AssignmentRevision: 3, AssignmentDigest: evidence.Digest([]byte("assignment")), Duration: 2 * time.Minute}
	_, digest, err := req.Freeze()
	require.NoError(t, err)
	renewed, err := coordination.RenewClaim(claim, claim.Token(), req.Duration, claim.At.Add(time.Second))
	require.NoError(t, err)
	return req, coordination.TaskRenewal{RequestDigest: digest, Claim: renewed, Fresh: true, Outbox: coordination.TaskRenewalOutbox{EventID: id.New(coordination.PrefixTaskRenewalEvent), Type: "task_renewed", ClaimID: claim.ID, PlanID: claim.PlanID, TaskID: claim.TaskID, RunID: claim.RunID}}
}

type taskRenewalStorage struct {
	coordination.TaskRenewalStore
	renew func(context.Context, coordination.TaskRenewalRequest) (coordination.TaskRenewal, error)
}

func (s taskRenewalStorage) RenewTaskExecution(ctx context.Context, r coordination.TaskRenewalRequest) (coordination.TaskRenewal, error) {
	return s.renew(ctx, r)
}

type taskRenewalFence func(context.Context, coordination.TaskRenewalRequest, func(coordination.TaskRenewalRequest) error) error

func (f taskRenewalFence) WithTaskRenewalFence(ctx context.Context, r coordination.TaskRenewalRequest, next func(coordination.TaskRenewalRequest) error) error {
	return f(ctx, r, next)
}

type taskRenewalAuthorizer func(context.Context, coordination.TaskRenewalRequest) error

func (f taskRenewalAuthorizer) AuthorizeTaskRenewal(ctx context.Context, r coordination.TaskRenewalRequest) error {
	return f(ctx, r)
}

func TestTaskRenewalBindsImmutableExecutionAndDeadline(t *testing.T) {
	req, receipt := renewalFixture(t)
	require.NoError(t, receipt.Validate(req))
	for _, mode := range []string{"actor", "reason", "schema", "held", "parent", "revision", "assignment", "duration", "claim_digest", "receipt_run", "receipt_outbox", "receipt_deadline"} {
		t.Run(mode, func(t *testing.T) {
			changed, _, err := req.Freeze()
			require.NoError(t, err)
			result := receipt
			switch mode {
			case "actor":
				changed.Actor = "another"
			case "reason":
				changed.Reason = ""
			case "schema":
				changed.SchemaVersion = 2
			case "held":
				changed.Claim.State = coordination.ClaimHeld
			case "parent":
				changed.ParentRunID = changed.Claim.RunID
			case "revision":
				changed.RunRevision = 0
			case "assignment":
				changed.AssignmentID = id.NewAgentRunID()
			case "duration":
				changed.Duration = coordination.MaxClaimDuration + time.Second
			case "claim_digest":
				changed.Claim.Digest = "changed"
			case "receipt_run":
				result.Claim.RunID = id.NewAgentRunID()
			case "receipt_outbox":
				result.Outbox.RunID = id.NewAgentRunID()
			case "receipt_deadline":
				result.Claim.ExpiresAt = result.Claim.Deadline.Add(time.Second)
			}
			require.Error(t, result.Validate(changed))
		})
	}
	frozen, _, err := req.Freeze()
	require.NoError(t, err)
	req.Claim.Scope.Levels[0].Value = "changed"
	require.NotEqual(t, req.Claim.Scope, frozen.Claim.Scope)
}

func TestTaskRenewerGuardsEveryAttemptAndDetachesPorts(t *testing.T) {
	req, receipt := renewalFixture(t)
	ctx := cortex.WithScope(context.Background(), req.Claim.Scope)
	held, allowed := false, true
	calls := 0
	fence := taskRenewalFence(func(_ context.Context, r coordination.TaskRenewalRequest, next func(coordination.TaskRenewalRequest) error) error {
		held = true
		defer func() { held = false }()
		return next(r)
	})
	authorizer := taskRenewalAuthorizer(func(_ context.Context, r coordination.TaskRenewalRequest) error {
		require.True(t, held)
		require.Equal(t, req, r)
		r.Claim.Scope.Levels[0].Value = "changed"
		if !allowed {
			return errors.New("revoked")
		}
		return nil
	})
	storage := taskRenewalStorage{renew: func(_ context.Context, r coordination.TaskRenewalRequest) (coordination.TaskRenewal, error) {
		require.True(t, held)
		require.Equal(t, req, r)
		calls++
		return receipt, nil
	}}
	renewer, err := coordination.NewTaskClaimRenewer(storage, fence, authorizer)
	require.NoError(t, err)
	result, err := renewer.Renew(ctx, req)
	require.NoError(t, err)
	require.True(t, result.Fresh)
	require.NoError(t, result.Validate(req))
	result.Claim.Scope.Levels[0].Value = "detached"
	require.NotEqual(t, result.Claim.Scope, receipt.Claim.Scope)
	receipt.Fresh = false
	result, err = renewer.Renew(ctx, req)
	require.NoError(t, err)
	require.False(t, result.Fresh)
	allowed = false
	_, err = renewer.Renew(ctx, req)
	require.Error(t, err)
	require.Equal(t, 2, calls)
}

func TestTaskRenewerRejectsChangedAuthorityAndInvalidReceipts(t *testing.T) {
	for _, mode := range []string{"changed", "missing_callback", "double_callback", "denied", "bad_receipt", "lost_ack", "cancelled", "scope"} {
		t.Run(mode, func(t *testing.T) {
			req, receipt := renewalFixture(t)
			ctx, cancel := context.WithCancel(cortex.WithScope(context.Background(), req.Claim.Scope))
			defer cancel()
			writes := 0
			fence := taskRenewalFence(func(_ context.Context, r coordination.TaskRenewalRequest, next func(coordination.TaskRenewalRequest) error) error {
				switch mode {
				case "changed":
					r.Duration++
				case "missing_callback":
					return nil
				case "cancelled":
					cancel()
				}
				err := next(r)
				if mode == "double_callback" {
					return errors.Join(err, next(r))
				}
				return err
			})
			authorizer := taskRenewalAuthorizer(func(context.Context, coordination.TaskRenewalRequest) error {
				if mode == "denied" {
					return errors.New("revoked")
				}
				return nil
			})
			storage := taskRenewalStorage{renew: func(context.Context, coordination.TaskRenewalRequest) (coordination.TaskRenewal, error) {
				writes++
				if mode == "bad_receipt" {
					receipt.RequestDigest = "bad"
				}
				if mode == "lost_ack" {
					return receipt, errors.New("lost acknowledgement")
				}
				return receipt, nil
			}}
			renewer, err := coordination.NewTaskClaimRenewer(storage, fence, authorizer)
			require.NoError(t, err)
			callCtx := ctx
			if mode == "scope" {
				callCtx = context.Background()
			}
			_, err = renewer.Renew(callCtx, req)
			require.Error(t, err)
			if mode == "bad_receipt" || mode == "lost_ack" || mode == "double_callback" {
				require.Equal(t, 1, writes)
			} else {
				require.Zero(t, writes)
			}
		})
	}
}

func TestTaskRenewerRequiresAllPorts(t *testing.T) {
	req, receipt := renewalFixture(t)
	storage := taskRenewalStorage{renew: func(context.Context, coordination.TaskRenewalRequest) (coordination.TaskRenewal, error) {
		return receipt, nil
	}}
	fence := taskRenewalFence(func(_ context.Context, r coordination.TaskRenewalRequest, next func(coordination.TaskRenewalRequest) error) error {
		return next(r)
	})
	authorizer := taskRenewalAuthorizer(func(context.Context, coordination.TaskRenewalRequest) error { return nil })
	_, err := coordination.NewTaskClaimRenewer(nil, fence, authorizer)
	require.ErrorIs(t, err, coordination.ErrInvalidClaim)
	_, err = coordination.NewTaskClaimRenewer(storage, taskRenewalFence(nil), authorizer)
	require.ErrorIs(t, err, coordination.ErrInvalidClaim)
	_, err = coordination.NewTaskClaimRenewer(storage, fence, taskRenewalAuthorizer(nil))
	require.ErrorIs(t, err, coordination.ErrInvalidClaim)
	require.NoError(t, receipt.Validate(req))
}
