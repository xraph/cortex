package coordination

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskCallSealRequest closes call admission for the exact accepted ownership
// revision. Closure neither proves external quiescence nor settles accounting.
type TaskCallSealRequest struct {
	SchemaVersion int                          `json:"schema_version"`
	Claim         ClaimToken                   `json:"claim"`
	ClaimDigest   string                       `json:"claim_digest"`
	Seal          execution.RunCallSealRequest `json:"seal"`
}

// Freeze validates and detaches task and journal bindings.
func (r TaskCallSealRequest) Freeze() (TaskCallSealRequest, string, error) {
	seal, _, err := r.Seal.Freeze()
	if err != nil || r.SchemaVersion != 1 || r.Claim.Validate() != nil || !evidence.ValidDigest(r.ClaimDigest) {
		return TaskCallSealRequest{}, "", execution.ErrInvalidRunCall
	}
	r.Seal = seal
	data, err := json.Marshal(r)
	if err != nil || len(data) > 32<<10 {
		return TaskCallSealRequest{}, "", execution.ErrInvalidRunCall
	}
	return r, evidence.Digest(data), nil
}

// TaskCallSealStore checks accepted ownership and the retained run while holding
// plan, run and journal guards through commit. Retries recheck accepted history;
// a replacement claim does not authorize calls on the old run.
type TaskCallSealStore interface {
	SealTaskCalls(context.Context, TaskCallSealRequest) (execution.RunCallSeal, error)
}

// TaskCallSealFence holds current host authority through one synchronous write.
type TaskCallSealFence interface {
	WithTaskCallSealFence(context.Context, TaskCallSealRequest, func(TaskCallSealRequest) error) error
}

// TaskCallSealAuthorizer authenticates the actor and exact task/run permission,
// including retries. A completed claim alone does not grant closure authority.
type TaskCallSealAuthorizer interface {
	AuthorizeTaskCallSeal(context.Context, TaskCallSealRequest) error
}

// TaskCallSealer closes accepted task admission under explicit host authority.
type TaskCallSealer interface {
	Seal(context.Context, TaskCallSealRequest) (execution.RunCallSeal, error)
}

type guardedTaskCallSealer struct {
	store      TaskCallSealStore
	fence      TaskCallSealFence
	authorizer TaskCallSealAuthorizer
}

// NewTaskCallSealer requires storage, current authorization and synchronous fencing.
// A failed acknowledgment can follow commit. Retry the identical request while
// accepted history remains valid; never infer that an error means calls remain open.
func NewTaskCallSealer(store TaskCallSealStore, fence TaskCallSealFence, authorizer TaskCallSealAuthorizer) (TaskCallSealer, error) {
	if hostport.IsNil(store) || hostport.IsNil(fence) || hostport.IsNil(authorizer) {
		return nil, execution.ErrInvalidRunCall
	}
	return &guardedTaskCallSealer{store: store, fence: fence, authorizer: authorizer}, nil
}

func (s *guardedTaskCallSealer) Seal(ctx context.Context, request TaskCallSealRequest) (execution.RunCallSeal, error) {
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	guard := taskAuthority[TaskCallSealRequest]{operation: "task call seal", invalid: execution.ErrInvalidRunCall, conflict: execution.ErrRunCallConflict, scope: func(r TaskCallSealRequest) cortex.Scope { return r.Seal.Scope }, fence: s.fence.WithTaskCallSealFence, authorize: s.authorizer.AuthorizeTaskCallSeal}
	return withTaskAuthority(bounded, request, guard, func(tc context.Context, frozen TaskCallSealRequest, _ string) (execution.RunCallSeal, error) {
		_, expected, err := frozen.Seal.Freeze()
		if err != nil {
			return execution.RunCallSeal{}, err
		}
		sealed, err := s.store.SealTaskCalls(tc, frozen)
		if err != nil {
			return execution.RunCallSeal{}, err
		}
		if sealed.Validate() != nil || sealed.RequestDigest != expected {
			return execution.RunCallSeal{}, execution.ErrRunCallConflict
		}
		sealed.Request, _, err = sealed.Request.Freeze()
		return sealed, err
	})
}
