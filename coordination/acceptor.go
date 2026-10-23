package coordination

import (
	"context"
	"fmt"

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
	if err := ctx.Err(); err != nil {
		return TaskAcceptance{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return TaskAcceptance{}, err
	}
	frozen, digest, err := request.Freeze()
	if err != nil {
		return TaskAcceptance{}, err
	}
	if frozen.Current.Scope.Canonical() != scope.Canonical() {
		return TaskAcceptance{}, ErrInvalidTaskAcceptance
	}
	fenceInput, _, err := frozen.Freeze()
	if err != nil {
		return TaskAcceptance{}, err
	}
	return hostport.WithSynchronousFence(ctx, ErrInvalidTaskAcceptance, func(attemptCtx context.Context, next func(TaskAcceptanceRequest) error) error {
		if fenceErr := a.fence.WithTaskAcceptanceFence(attemptCtx, fenceInput, next); fenceErr != nil {
			return fmt.Errorf("cortex: task acceptance fence: %w", fenceErr)
		}
		return nil
	}, func(attemptCtx context.Context, current TaskAcceptanceRequest) (TaskAcceptance, error) {
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return TaskAcceptance{}, contextErr
		}
		authorized, currentDigest, freezeErr := current.Freeze()
		if freezeErr != nil {
			return TaskAcceptance{}, freezeErr
		}
		if currentDigest != digest {
			return TaskAcceptance{}, ErrTaskAcceptanceConflict
		}
		if authorizeErr := a.authorizer.AuthorizeTaskAcceptance(attemptCtx, authorized); authorizeErr != nil {
			return TaskAcceptance{}, fmt.Errorf("cortex: authorize task acceptance: %w", authorizeErr)
		}
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return TaskAcceptance{}, contextErr
		}
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
