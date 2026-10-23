package coordination

import (
	"context"
	"encoding/json"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskDispatchFence resolves current host authority and prerequisite bindings,
// then holds their grant and external revision guards through the callback.
// Invoke it once, synchronously. Every relevant writer, including other
// processes, must participate. External services require conditional operations
// or a declared bounded observation window; an unlocked read is insufficient.
type TaskDispatchFence interface {
	WithTaskDispatchFence(context.Context, TaskDispatchRequest, func(TaskDispatchRequest) error) error
}

// TaskDispatchAuthorizer authenticates Actor and Worker, resolves the retained
// admission, assignment and parent, and checks their current attenuated grant,
// allocation and complete prerequisite chain. Stored identities and digests are
// references, never credentials or proof of current permission.
type TaskDispatchAuthorizer interface {
	AuthorizeTaskDispatch(context.Context, TaskDispatchRequest) error
}

// TaskDispatchAdmitter guards durable dispatch intent with current authority.
// It does not invoke a worker. Every retry repeats authorization; an error may
// follow a durable commit, so preserve the original request when recovering.
type TaskDispatchAdmitter interface {
	Admit(context.Context, TaskDispatchRequest) (TaskDispatch, error)
}

type guardedTaskDispatchAdmitter struct {
	storage    TaskDispatchStore
	fence      TaskDispatchFence
	authorizer TaskDispatchAuthorizer
}

// NewTaskDispatchAdmitter requires explicit storage, fencing and authorization.
func NewTaskDispatchAdmitter(storage TaskDispatchStore, fence TaskDispatchFence, authorizer TaskDispatchAuthorizer) (TaskDispatchAdmitter, error) {
	if hostport.IsNil(storage) || hostport.IsNil(fence) || hostport.IsNil(authorizer) {
		return nil, ErrInvalidClaim
	}
	return &guardedTaskDispatchAdmitter{storage: storage, fence: fence, authorizer: authorizer}, nil
}

func (a *guardedTaskDispatchAdmitter) Admit(ctx context.Context, request TaskDispatchRequest) (TaskDispatch, error) {
	guard := taskAuthority[TaskDispatchRequest]{operation: "task dispatch", invalid: ErrInvalidClaim, conflict: ErrClaimConflict,
		scope: func(req TaskDispatchRequest) cortex.Scope { return req.Scope },
		fence: a.fence.WithTaskDispatchFence, authorize: a.authorizer.AuthorizeTaskDispatch,
	}
	return withTaskAuthority(ctx, request, guard, func(attemptCtx context.Context, frozen TaskDispatchRequest, digest string) (TaskDispatch, error) {
		receipt, err := a.storage.DispatchReadyTask(attemptCtx, frozen)
		if err != nil {
			return TaskDispatch{}, err
		}
		if receipt.RequestDigest != digest || receipt.Validate(frozen) != nil {
			return TaskDispatch{}, ErrInvalidClaim
		}
		data, err := json.Marshal(receipt)
		if err != nil || len(data) > 1<<20 {
			return TaskDispatch{}, ErrInvalidClaim
		}
		var detached TaskDispatch
		if json.Unmarshal(data, &detached) != nil {
			return TaskDispatch{}, ErrInvalidClaim
		}
		detached.Fresh = receipt.Fresh
		return detached, nil
	})
}
