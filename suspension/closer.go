package suspension

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/internal/hostport"
)

// ResumeClosureFence holds current run/team grants and permanent quiescence of
// every original worker and tool through one synchronous callback. Include remote
// workers, pending callbacks and delayed external actions. Stopping one process
// is sufficient only when the host proves it owned all possible execution.
type ResumeClosureFence interface {
	WithResumeClosure(context.Context, ResumeClosureRequest, func(ResumeClosureRequest) error) error
}

// ResumeClosureAuthorizer authenticates the actor, proof issuer/revision and
// proof contents bound to the exact request, then checks current run/team access.
// Original worker identities must come from retained dispatch evidence. Current
// agent configuration cannot replace them. Recheck access and original proof on
// historical retries; a stored receipt or a hash is not permission.
type ResumeClosureAuthorizer interface {
	AuthorizeResumeClosure(context.Context, ResumeClosureRequest) error
}

// ResumeCloser terminates uncertain execution as incomplete under host guards.
// It neither retries work nor accepts successful results. Errors can follow a
// commit; retry identical contents to recover the original durable receipt.
type ResumeCloser interface {
	Close(context.Context, ResumeClosureRequest) (ResumeClosure, error)
}

type guardedResumeCloser struct {
	store      ResumeClosureStore
	fence      ResumeClosureFence
	authorizer ResumeClosureAuthorizer
}

// NewResumeCloser requires explicit host proof authentication and fencing.
// Each operation is bounded to thirty seconds. Host ports must honor cancellation.
func NewResumeCloser(storage ResumeClosureStore, fence ResumeClosureFence, authorizer ResumeClosureAuthorizer) (ResumeCloser, error) {
	if hostport.IsNil(storage) || hostport.IsNil(fence) || hostport.IsNil(authorizer) {
		return nil, ErrResumeAdmissionUnavailable
	}
	return &guardedResumeCloser{store: storage, fence: fence, authorizer: authorizer}, nil
}

func (c *guardedResumeCloser) Close(ctx context.Context, request ResumeClosureRequest) (ResumeClosure, error) {
	if err := ctx.Err(); err != nil {
		return ResumeClosure{}, err
	}
	r, digest, err := request.Freeze()
	if err != nil {
		return ResumeClosure{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return ResumeClosure{}, err
	}
	if scope.Canonical() != r.Scope.Canonical() {
		return ResumeClosure{}, cortex.ErrInvalidScope
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return hostport.WithSynchronousFence(bounded, ErrResumeAdmissionConflict, func(attemptCtx context.Context, next func(ResumeClosureRequest) error) error {
		return c.fence.WithResumeClosure(attemptCtx, cloneClosureRequest(r), next)
	}, func(attemptCtx context.Context, current ResumeClosureRequest) (ResumeClosure, error) {
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return ResumeClosure{}, contextErr
		}
		authorized, currentDigest, freezeErr := current.Freeze()
		if freezeErr != nil || currentDigest != digest {
			return ResumeClosure{}, ErrResumeAdmissionConflict
		}
		if authErr := c.authorizer.AuthorizeResumeClosure(attemptCtx, authorized); authErr != nil {
			return ResumeClosure{}, authErr
		}
		if contextErr := attemptCtx.Err(); contextErr != nil {
			return ResumeClosure{}, contextErr
		}
		receipt, closeErr := c.store.CloseResume(attemptCtx, cloneClosureRequest(r))
		if closeErr != nil {
			return ResumeClosure{}, closeErr
		}
		if receipt.Validate() != nil || receipt.RequestDigest != digest {
			return ResumeClosure{}, ErrResumeAdmissionConflict
		}
		receipt.Request = cloneClosureRequest(receipt.Request)
		return receipt, nil
	})
}

func cloneClosureRequest(r ResumeClosureRequest) ResumeClosureRequest {
	r.Scope.Levels = append([]cortex.Level(nil), r.Scope.Levels...)
	return r
}
