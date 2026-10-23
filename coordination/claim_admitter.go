package coordination

import (
	"context"
	"encoding/json"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskClaimFence holds current grant and external revision guards through ready
// task admission. Resolve the complete prerequisite chain and invoke the callback
// once, synchronously. All relevant writers, including other processes, must
// participate. External services need conditional operations or an explicit
// bounded observation window; an unlocked read does not implement this port.
type TaskClaimFence interface {
	WithTaskClaimFence(context.Context, ReadyTaskClaimRequest, func(ReadyTaskClaimRequest) error) error
}

// TaskClaimAuthorizer authenticates the caller and requested worker, checks its
// current attenuated grant and allocation, and authorizes every prerequisite.
// Identity references, grant names and receipt digests are not credentials.
type TaskClaimAuthorizer interface {
	AuthorizeTaskClaim(context.Context, ReadyTaskClaimRequest) error
}

// TaskClaimAdmitter guards atomic admission with current host authority. Every
// retry repeats authorization. An error can follow a durable commit; retry the
// same request identity and never infer dispatch permission from an acknowledgment.
type TaskClaimAdmitter interface {
	Admit(context.Context, ReadyTaskClaimRequest) (ReadyTaskClaim, error)
}

type guardedTaskClaimAdmitter struct {
	storage    ReadyTaskClaimStore
	fence      TaskClaimFence
	authorizer TaskClaimAuthorizer
}

// NewTaskClaimAdmitter requires explicit storage, fencing and authorization ports.
func NewTaskClaimAdmitter(storage ReadyTaskClaimStore, fence TaskClaimFence, authorizer TaskClaimAuthorizer) (TaskClaimAdmitter, error) {
	if hostport.IsNil(storage) || hostport.IsNil(fence) || hostport.IsNil(authorizer) {
		return nil, ErrInvalidClaim
	}
	return &guardedTaskClaimAdmitter{storage: storage, fence: fence, authorizer: authorizer}, nil
}

func (a *guardedTaskClaimAdmitter) Admit(ctx context.Context, request ReadyTaskClaimRequest) (ReadyTaskClaim, error) {
	guard := taskAuthority[ReadyTaskClaimRequest]{
		operation: "task claim", invalid: ErrInvalidClaim, conflict: ErrClaimConflict,
		scope: func(req ReadyTaskClaimRequest) cortex.Scope { return req.Acquisition.Reservation.Scope },
		fence: a.fence.WithTaskClaimFence, authorize: a.authorizer.AuthorizeTaskClaim,
	}
	return withTaskAuthority(ctx, request, guard, func(attemptCtx context.Context, frozen ReadyTaskClaimRequest, digest string) (ReadyTaskClaim, error) {
		receipt, err := a.storage.ClaimReadyTask(attemptCtx, frozen)
		if err != nil {
			return ReadyTaskClaim{}, err
		}
		if receipt.RequestDigest != digest || receipt.Validate(frozen) != nil {
			return ReadyTaskClaim{}, ErrInvalidClaim
		}
		data, err := json.Marshal(receipt)
		if err != nil || len(data) > 128<<10 {
			return ReadyTaskClaim{}, ErrInvalidClaim
		}
		var detached ReadyTaskClaim
		if json.Unmarshal(data, &detached) != nil {
			return ReadyTaskClaim{}, ErrInvalidClaim
		}
		detached.Fresh = receipt.Fresh
		return detached, nil
	})
}
