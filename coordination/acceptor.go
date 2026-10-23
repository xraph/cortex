package coordination

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskAcceptanceFence holds the host's grant and external revision guards for
// the task and its complete prerequisite chain until acceptance returns. Resolve
// current bindings from authoritative sources and invoke the callback exactly
// once, synchronously. All relevant mutations, including other processes, must
// participate in those guards. An unlocked read does not implement this port.
// External services need conditional operations or an explicitly bounded
// observation window; this interface cannot create cross-service atomicity.
type TaskAcceptanceFence interface {
	WithTaskAcceptanceFence(context.Context, TaskAcceptanceRequest, func(TaskAcceptanceRequest) error) error
}

// TaskAcceptanceAuthorizer authenticates Actor and authorizes the complete task,
// claim, assignment and dependency request against current host policy. A stored
// principal, claim token or artifact digest is not a credential.
type TaskAcceptanceAuthorizer interface {
	AuthorizeTaskAcceptance(context.Context, TaskAcceptanceRequest) error
}

// TaskAcceptor checks current host authority before atomic task storage. Even
// historical retries require authorization. An error may follow a durable commit;
// recover with the same request ID rather than creating another acceptance.
type TaskAcceptor interface {
	Accept(context.Context, TaskAcceptanceRequest) (TaskAcceptance, error)
}

type guardedTaskAcceptor struct {
	storage    TaskAcceptanceStore
	fence      TaskAcceptanceFence
	authorizer TaskAcceptanceAuthorizer
}

// NewTaskAcceptor requires explicit storage, fencing and authorization ports.
func NewTaskAcceptor(storage TaskAcceptanceStore, fence TaskAcceptanceFence, authorizer TaskAcceptanceAuthorizer) (TaskAcceptor, error) {
	if hostport.IsNil(storage) || hostport.IsNil(fence) || hostport.IsNil(authorizer) {
		return nil, ErrInvalidTaskAcceptance
	}
	return &guardedTaskAcceptor{storage: storage, fence: fence, authorizer: authorizer}, nil
}

func (a *guardedTaskAcceptor) Accept(ctx context.Context, request TaskAcceptanceRequest) (TaskAcceptance, error) {
	guard := taskAuthority[TaskAcceptanceRequest]{
		operation: "task acceptance", invalid: ErrInvalidTaskAcceptance, conflict: ErrTaskAcceptanceConflict,
		scope: func(req TaskAcceptanceRequest) cortex.Scope { return req.Current.Scope },
		fence: a.fence.WithTaskAcceptanceFence, authorize: a.authorizer.AuthorizeTaskAcceptance,
	}
	return withTaskAuthority(ctx, request, guard, func(attemptCtx context.Context, frozen TaskAcceptanceRequest, digest string) (TaskAcceptance, error) {
		receipt, acceptErr := a.storage.AcceptTaskResult(attemptCtx, frozen)
		if acceptErr != nil {
			return TaskAcceptance{}, acceptErr
		}
		if receipt.Validate() != nil || receipt.RequestDigest != digest {
			return TaskAcceptance{}, ErrInvalidTaskAcceptance
		}
		detached, sealErr := SealTaskAcceptance(receipt)
		if sealErr != nil {
			return TaskAcceptance{}, sealErr
		}
		detached.Fresh = receipt.Fresh
		return detached, nil
	})
}
