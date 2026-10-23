package coordination

import (
	"context"
	"encoding/json"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskRunFence resolves current host authority and prerequisite bindings, then
// holds their grant and external revision guards through the callback. Invoke
// it once, synchronously. Every relevant writer, including other processes,
// must participate. External services need conditional operations or a declared
// bounded observation window; an unlocked read is insufficient.
type TaskRunFence interface {
	WithTaskRunFence(context.Context, TaskRunRequest, func(TaskRunRequest) error) error
}

// TaskRunAuthorizer authenticates Actor and Worker and resolves their retained
// dispatch, assignment and parent. It checks the current attenuated grant,
// allocation, complete prerequisite chain, prepared configuration and capture
// policy. Stored identities and digests are references, never credentials.
type TaskRunAuthorizer interface {
	AuthorizeTaskRun(context.Context, TaskRunRequest) error
}

// TaskRunAdmitter guards atomic child creation with current host authority.
// It does not execute the child. Every retry repeats authorization, including
// historical acknowledgments. Preserve the request after an ambiguous failure.
type TaskRunAdmitter interface {
	Admit(context.Context, TaskRunRequest) (TaskRunStart, error)
}

type guardedTaskRunAdmitter struct {
	storage    TaskRunStore
	fence      TaskRunFence
	authorizer TaskRunAuthorizer
}

// NewTaskRunAdmitter requires explicit storage, fencing and authorization.
func NewTaskRunAdmitter(storage TaskRunStore, fence TaskRunFence, authorizer TaskRunAuthorizer) (TaskRunAdmitter, error) {
	if hostport.IsNil(storage) || hostport.IsNil(fence) || hostport.IsNil(authorizer) {
		return nil, ErrInvalidClaim
	}
	return &guardedTaskRunAdmitter{storage: storage, fence: fence, authorizer: authorizer}, nil
}

func (a *guardedTaskRunAdmitter) Admit(ctx context.Context, request TaskRunRequest) (TaskRunStart, error) {
	guard := taskAuthority[TaskRunRequest]{operation: "task run creation", invalid: ErrInvalidClaim, conflict: ErrClaimConflict,
		scope: func(req TaskRunRequest) cortex.Scope { return req.Run.Scope },
		fence: a.fence.WithTaskRunFence, authorize: a.authorizer.AuthorizeTaskRun,
	}
	return withTaskAuthority(ctx, request, guard, func(attemptCtx context.Context, frozen TaskRunRequest, digest string) (TaskRunStart, error) {
		receipt, err := a.storage.CreateTaskRun(attemptCtx, frozen)
		if err != nil {
			return TaskRunStart{}, err
		}
		if receipt.RequestDigest != digest || receipt.Validate(frozen) != nil {
			return TaskRunStart{}, ErrInvalidClaim
		}
		data, err := json.Marshal(receipt)
		if err != nil || len(data) > 64<<10 {
			return TaskRunStart{}, ErrInvalidClaim
		}
		var detached TaskRunStart
		if json.Unmarshal(data, &detached) != nil {
			return TaskRunStart{}, ErrInvalidClaim
		}
		detached.Fresh = receipt.Fresh
		return detached, nil
	})
}
