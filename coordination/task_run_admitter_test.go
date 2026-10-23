package coordination_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

type taskRunFenceFunc func(context.Context, coordination.TaskRunRequest, func(coordination.TaskRunRequest) error) error

func (f taskRunFenceFunc) WithTaskRunFence(ctx context.Context, req coordination.TaskRunRequest, next func(coordination.TaskRunRequest) error) error {
	return f(ctx, req, next)
}

type taskRunAuthorizeFunc func(context.Context, coordination.TaskRunRequest) error

func (f taskRunAuthorizeFunc) AuthorizeTaskRun(ctx context.Context, req coordination.TaskRunRequest) error {
	return f(ctx, req)
}

type taskRunStorage struct {
	coordination.TaskRunStore
	create func(context.Context, coordination.TaskRunRequest) (coordination.TaskRunStart, error)
}

func (s taskRunStorage) CreateTaskRun(ctx context.Context, req coordination.TaskRunRequest) (coordination.TaskRunStart, error) {
	return s.create(ctx, req)
}

func taskRunStartFixture(t *testing.T, req coordination.TaskRunRequest) coordination.TaskRunStart {
	t.Helper()
	_, digest, err := req.Freeze()
	require.NoError(t, err)
	child, err := req.InitialRun(req.Intent.Template.Binding.Candidate.CreatedAt.Add(time.Second))
	require.NoError(t, err)
	data, err := json.Marshal(child)
	require.NoError(t, err)
	return coordination.TaskRunStart{RequestDigest: digest, RunID: child.ID, RunDigest: evidence.Digest(data), CreatedAt: child.CreatedAt, IntentID: req.Intent.ID, IntentDigest: req.Intent.Digest, Outbox: coordination.TaskRunOutbox{EventID: id.New(coordination.PrefixTaskRunEvent), Type: "task_run_created", ClaimID: req.Claim.ClaimID, PlanID: req.Claim.PlanID, TaskID: req.Claim.TaskID, RunID: child.ID, IntentID: req.Intent.ID}}
}

func TestTaskRunAdmitterHoldsAuthorityThroughCreation(t *testing.T) {
	req := taskRunRequestFixture(t)
	receipt := taskRunStartFixture(t, req)
	receipt.Fresh = true
	var steps []string
	held := false
	fence := taskRunFenceFunc(func(_ context.Context, current coordination.TaskRunRequest, next func(coordination.TaskRunRequest) error) error {
		held = true
		steps = append(steps, "acquire")
		defer func() { held = false; steps = append(steps, "release") }()
		return next(current)
	})
	authorizer := taskRunAuthorizeFunc(func(_ context.Context, current coordination.TaskRunRequest) error {
		require.True(t, held)
		require.Equal(t, req, current)
		steps = append(steps, "authorize")
		current.Run.Scope.Levels[0].Value = "mutated"
		current.Intent.Template.Binding.Context.ExecutionInputs.SHA256 = evidence.Digest(nil)
		return nil
	})
	storage := taskRunStorage{create: func(_ context.Context, current coordination.TaskRunRequest) (coordination.TaskRunStart, error) {
		require.True(t, held)
		require.Equal(t, req, current)
		steps = append(steps, "create")
		return receipt, nil
	}}
	admitter, err := coordination.NewTaskRunAdmitter(storage, fence, authorizer)
	require.NoError(t, err)
	got, err := admitter.Admit(cortex.WithScope(context.Background(), req.Run.Scope), req)
	require.NoError(t, err)
	require.Equal(t, receipt, got)
	require.Equal(t, []string{"acquire", "authorize", "create", "release"}, steps)
	require.NoError(t, got.Validate(req))
}

func TestTaskRunAdmitterRejectsChangedAuthorityAndReceipts(t *testing.T) {
	for _, mode := range []string{"denied", "changed_input", "changed_parent", "changed_assignment", "changed_budget", "changed_policy", "wrong_scope", "missing_scope", "cancelled", "cancel_in_authorizer", "missing_callback", "duplicate_callback", "lost_ack", "store_error", "bad_receipt", "unrelated_receipt", "forged_digest"} {
		t.Run(mode, func(t *testing.T) {
			req := taskRunRequestFixture(t)
			receipt := taskRunStartFixture(t, req)
			ctx := cortex.WithScope(context.Background(), req.Run.Scope)
			if mode == "missing_scope" {
				ctx = context.Background()
			}
			if mode == "wrong_scope" {
				ctx = cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
			}
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			if mode == "cancelled" {
				cancel()
			}
			calls := 0
			storage := taskRunStorage{create: func(context.Context, coordination.TaskRunRequest) (coordination.TaskRunStart, error) {
				calls++
				switch mode {
				case "store_error":
					return coordination.TaskRunStart{}, errors.New("storage failed")
				case "bad_receipt":
					receipt.Outbox.Type = "completed"
				case "unrelated_receipt", "forged_digest":
					expected := receipt.RequestDigest
					receipt = taskRunStartFixture(t, taskRunRequestFixture(t))
					if mode == "forged_digest" {
						receipt.RequestDigest = expected
					}
				}
				return receipt, nil
			}}
			fence := taskRunFenceFunc(func(_ context.Context, current coordination.TaskRunRequest, next func(coordination.TaskRunRequest) error) error {
				switch mode {
				case "changed_input":
					current.Run.Input = "replacement"
				case "changed_parent":
					current.ParentRunRevision++
				case "changed_assignment":
					current.AssignmentRevision++
				case "changed_budget":
					current.Run.Budget.MaxTokens++
				case "changed_policy":
					current.Intent.MaxCandidateBytes++
				case "missing_callback":
					return nil
				case "duplicate_callback":
					_ = next(current)
					_ = next(current)
					return nil
				case "lost_ack":
					_ = next(current)
					return errors.New("lost acknowledgment")
				}
				return next(current)
			})
			authorizer := taskRunAuthorizeFunc(func(context.Context, coordination.TaskRunRequest) error {
				if mode == "denied" {
					return errors.New("revoked")
				}
				if mode == "cancel_in_authorizer" {
					cancel()
				}
				return nil
			})
			admitter, err := coordination.NewTaskRunAdmitter(storage, fence, authorizer)
			require.NoError(t, err)
			got, err := admitter.Admit(ctx, req)
			require.Error(t, err)
			require.Zero(t, got)
			expected := 0
			switch mode {
			case "duplicate_callback", "lost_ack", "store_error", "bad_receipt", "unrelated_receipt", "forged_digest":
				expected = 1
			}
			require.Equal(t, expected, calls)
		})
	}
}

func TestTaskRunAdmitterReauthorizesHistoricalCreation(t *testing.T) {
	req := taskRunRequestFixture(t)
	accepted := taskAcceptanceFixture(t)
	accepted.Request.Current.Scope = req.Run.Scope
	req.Dependencies = []coordination.AcceptedDependency{{AcceptanceID: accepted.Request.ID, Digest: accepted.Digest, Current: accepted.Request.Current}}
	receipt := taskRunStartFixture(t, req)
	allowed, stale := true, false
	calls, authorizations := 0, 0
	fence := taskRunFenceFunc(func(_ context.Context, current coordination.TaskRunRequest, next func(coordination.TaskRunRequest) error) error {
		if stale {
			current.Dependencies[0].Current.Context.AuthorityRevision = "revoked"
		}
		return next(current)
	})
	authorizer := taskRunAuthorizeFunc(func(_ context.Context, current coordination.TaskRunRequest) error {
		authorizations++
		require.Equal(t, req.Dependencies, current.Dependencies)
		if !allowed {
			return errors.New("revoked")
		}
		return nil
	})
	storage := taskRunStorage{create: func(context.Context, coordination.TaskRunRequest) (coordination.TaskRunStart, error) {
		calls++
		return receipt, nil
	}}
	admitter, err := coordination.NewTaskRunAdmitter(storage, fence, authorizer)
	require.NoError(t, err)
	ctx := cortex.WithScope(context.Background(), req.Run.Scope)
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

func TestTaskRunAdmitterRequiresPorts(t *testing.T) {
	fence := taskRunFenceFunc(func(_ context.Context, current coordination.TaskRunRequest, next func(coordination.TaskRunRequest) error) error {
		return next(current)
	})
	authorizer := taskRunAuthorizeFunc(func(context.Context, coordination.TaskRunRequest) error { return nil })
	for _, mode := range []string{"storage", "fence", "authorizer"} {
		var s coordination.TaskRunStore = taskRunStorage{}
		var f coordination.TaskRunFence = fence
		var a coordination.TaskRunAuthorizer = authorizer
		switch mode {
		case "storage":
			s = (*taskRunStorage)(nil)
		case "fence":
			f = taskRunFenceFunc(nil)
		case "authorizer":
			a = taskRunAuthorizeFunc(nil)
		}
		_, err := coordination.NewTaskRunAdmitter(s, f, a)
		require.Error(t, err)
	}
}
