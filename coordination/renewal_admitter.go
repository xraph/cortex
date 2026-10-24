package coordination

import (
	"context"
	"encoding/json"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskRenewalFence holds current grants and external prerequisite revisions
// through the callback. Every relevant writer must participate. Invoke next
// exactly once and synchronously; an unlocked read does not implement this port.
type TaskRenewalFence interface {
	WithTaskRenewalFence(context.Context, TaskRenewalRequest, func(TaskRenewalRequest) error) error
}

// TaskRenewalAuthorizer authenticates the worker and checks its current
// attenuated grant, parent, child, assignment, budget and prerequisite access.
// Waiting work may retain ownership, but renewal never grants resume permission.
type TaskRenewalAuthorizer interface {
	AuthorizeTaskRenewal(context.Context, TaskRenewalRequest) error
}

// TaskClaimRenewer guards durable execution renewal with current host authority.
// Errors can follow a committed renewal. Reconcile rather than inferring a new
// expiry; historical acknowledgements never authorize continued execution.
type TaskClaimRenewer interface {
	Renew(context.Context, TaskRenewalRequest) (TaskRenewal, error)
}

type guardedTaskClaimRenewer struct {
	storage    TaskRenewalStore
	fence      TaskRenewalFence
	authorizer TaskRenewalAuthorizer
}

// NewTaskClaimRenewer requires explicit storage, fencing and authorization ports.
func NewTaskClaimRenewer(storage TaskRenewalStore, fence TaskRenewalFence, authorizer TaskRenewalAuthorizer) (TaskClaimRenewer, error) {
	if hostport.IsNil(storage) || hostport.IsNil(fence) || hostport.IsNil(authorizer) {
		return nil, ErrInvalidClaim
	}
	return &guardedTaskClaimRenewer{storage: storage, fence: fence, authorizer: authorizer}, nil
}

func (a *guardedTaskClaimRenewer) Renew(ctx context.Context, request TaskRenewalRequest) (TaskRenewal, error) {
	guard := taskAuthority[TaskRenewalRequest]{operation: "task renewal", invalid: ErrInvalidClaim, conflict: ErrClaimConflict, scope: func(req TaskRenewalRequest) cortex.Scope { return req.Claim.Scope }, fence: a.fence.WithTaskRenewalFence, authorize: a.authorizer.AuthorizeTaskRenewal}
	return withTaskAuthority(ctx, request, guard, func(attemptCtx context.Context, frozen TaskRenewalRequest, digest string) (TaskRenewal, error) {
		receipt, err := a.storage.RenewTaskExecution(attemptCtx, frozen)
		if err != nil {
			return TaskRenewal{}, err
		}
		if receipt.RequestDigest != digest || receipt.Validate(frozen) != nil {
			return TaskRenewal{}, ErrInvalidClaim
		}
		data, err := json.Marshal(receipt)
		if err != nil || len(data) > 128<<10 {
			return TaskRenewal{}, ErrInvalidClaim
		}
		var detached TaskRenewal
		if json.Unmarshal(data, &detached) != nil {
			return TaskRenewal{}, ErrInvalidClaim
		}
		detached.Fresh = receipt.Fresh
		return detached, nil
	})
}
