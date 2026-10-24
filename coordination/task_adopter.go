package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskRecoveryFence holds current host grants, source revisions and permanent
// worker/tool quiescence through one synchronous adoption callback. Its guards
// must cover every relevant writer, including remote processes. Authenticate
// original dispatch identities; current agent configuration cannot replace them.
// Never hold a database transaction across external proof retrieval.
type TaskRecoveryFence interface {
	WithTaskRecoveryFence(context.Context, TaskAcceptanceRequest, func(TaskAcceptanceRequest) error) error
}

// TaskRecoveryAuthorizer authenticates Actor and the Recovery issuer, revision
// and proof content. The attestation must bind the exact claim/seal/snapshot and
// authenticate all original provider and effect receipts, prove permanent worker
// and tool quiescence, and cover delayed delivery. It also authorizes the current
// result and complete prerequisite chain. Hashes and fixture refs are not proof.
// Historical retries still require current access and authentic original proof.
type TaskRecoveryAuthorizer interface {
	AuthorizeTaskRecovery(context.Context, TaskAcceptanceRequest) error
}

// TaskRecoveryAdopter adopts verified output after an expired dispatch. Errors
// may follow a commit: retry the identical request ID to recover its receipt.
type TaskRecoveryAdopter interface {
	Adopt(context.Context, TaskAcceptanceRequest) (TaskAcceptance, error)
}

type guardedTaskRecoveryAdopter struct {
	storage    TaskRecoveryStore
	fence      TaskRecoveryFence
	authorizer TaskRecoveryAuthorizer
}

// NewTaskRecoveryAdopter requires explicit recovery authentication and fencing.
// Ordinary TaskAcceptor ports cannot authorize this separate transition.
func NewTaskRecoveryAdopter(storage TaskRecoveryStore, fence TaskRecoveryFence, authorizer TaskRecoveryAuthorizer) (TaskRecoveryAdopter, error) {
	if hostport.IsNil(storage) || hostport.IsNil(fence) || hostport.IsNil(authorizer) {
		return nil, ErrInvalidTaskAcceptance
	}
	return &guardedTaskRecoveryAdopter{storage: storage, fence: fence, authorizer: authorizer}, nil
}

func (a *guardedTaskRecoveryAdopter) Adopt(ctx context.Context, request TaskAcceptanceRequest) (TaskAcceptance, error) {
	if request.Recovery == nil {
		return TaskAcceptance{}, ErrInvalidTaskAcceptance
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	guard := taskAuthority[TaskAcceptanceRequest]{operation: "task recovery adoption", invalid: ErrInvalidTaskAcceptance, conflict: ErrTaskAcceptanceConflict, scope: func(r TaskAcceptanceRequest) cortex.Scope { return r.Current.Scope }, fence: a.fence.WithTaskRecoveryFence, authorize: a.authorizer.AuthorizeTaskRecovery}
	return withTaskAuthority(bounded, request, guard, func(attemptCtx context.Context, frozen TaskAcceptanceRequest, digest string) (TaskAcceptance, error) {
		receipt, err := a.storage.AdoptTaskResult(attemptCtx, frozen)
		if err != nil {
			return TaskAcceptance{}, err
		}
		return checkedTaskAcceptance(receipt, digest)
	})
}
