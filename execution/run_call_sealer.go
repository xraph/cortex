package execution

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
)

// RunCallSealFence holds current host authority through one synchronous callback.
// It must not substitute another request. The seal closes journal membership;
// stopping already admitted external work still requires its call-lease controls.
type RunCallSealFence interface {
	WithRunCallSeal(context.Context, RunCallSealRequest, func(RunCallSealRequest) error) error
}

// RunCallSealAuthorizer authenticates the closure actor and current exact-run
// permission, including on historical retries. Stored references grant no access.
type RunCallSealAuthorizer interface {
	AuthorizeRunCallSeal(context.Context, RunCallSealRequest) error
}

// RunCallSealer closes call admission under current host guards. It never settles
// usage, accepts results or infers that already admitted calls have finished.
type RunCallSealer interface {
	Seal(context.Context, RunCallSealRequest) (RunCallSeal, error)
}

type guardedRunCallSealer struct {
	store      RunCallSealStore
	fence      RunCallSealFence
	authorizer RunCallSealAuthorizer
}

// NewRunCallSealer requires explicit storage, authority fencing and authorization.
// Each operation has a thirty-second deadline; all host ports must honor it.
func NewRunCallSealer(store RunCallSealStore, fence RunCallSealFence, authorizer RunCallSealAuthorizer) (RunCallSealer, error) {
	if hostport.IsNil(store) || hostport.IsNil(fence) || hostport.IsNil(authorizer) {
		return nil, ErrInvalidRunCall
	}
	return &guardedRunCallSealer{store: store, fence: fence, authorizer: authorizer}, nil
}

func (s *guardedRunCallSealer) Seal(ctx context.Context, request RunCallSealRequest) (RunCallSeal, error) {
	if err := ctx.Err(); err != nil {
		return RunCallSeal{}, err
	}
	r, digest, err := request.Freeze()
	if err != nil {
		return RunCallSeal{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return RunCallSeal{}, err
	}
	if scope.Canonical() != r.Scope.Canonical() {
		return RunCallSeal{}, cortex.ErrInvalidScope
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// The fence and authorizer receive detached scope slices. Keep the original
	// frozen binding private for the storage call and returned receipt validation.
	offered := cloneSealRequest(r)
	return hostport.WithSynchronousFence(bounded, ErrInvalidRunCall, func(attemptCtx context.Context, next func(RunCallSealRequest) error) error {
		return s.fence.WithRunCallSeal(attemptCtx, offered, next)
	}, func(attemptCtx context.Context, current RunCallSealRequest) (RunCallSeal, error) {
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return RunCallSeal{}, contextErr
		}
		authorized, currentDigest, freezeErr := current.Freeze()
		if freezeErr != nil {
			return RunCallSeal{}, freezeErr
		}
		if currentDigest != digest {
			return RunCallSeal{}, ErrRunCallConflict
		}
		if authErr := s.authorizer.AuthorizeRunCallSeal(attemptCtx, authorized); authErr != nil {
			return RunCallSeal{}, authErr
		}
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return RunCallSeal{}, contextErr
		}
		result, sealErr := s.store.SealRunCalls(attemptCtx, cloneSealRequest(r))
		if sealErr != nil {
			return RunCallSeal{}, sealErr
		}
		if result.Validate() != nil || result.RequestDigest != digest {
			return RunCallSeal{}, ErrInvalidRunCall
		}
		result.Request = cloneSealRequest(result.Request)
		return result, nil
	})
}

func cloneSealRequest(r RunCallSealRequest) RunCallSealRequest {
	r.Scope.Levels = append([]cortex.Level(nil), r.Scope.Levels...)
	return r
}
