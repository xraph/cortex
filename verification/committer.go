package verification

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/xraph/cortex"
)

// CommitFence holds the host's authorization and revision guards while commit
// runs. It must capture current state from authoritative sources, call commit
// exactly once and synchronously, and keep the guards until commit returns.
// All relevant grant and local revision mutations must participate in the same
// guards, including mutations from other processes. A read followed by an
// unlocked callback does not implement this contract. There is no default fence.
//
// External mutable state needs a version-conditional operation or an explicitly
// bounded observation window. This interface cannot create atomicity across
// independent services. A host unable to meet its contract must return an error.
type CommitFence interface {
	WithCommitFence(context.Context, Binding, func(Binding) error) error
}

// Committer authorizes a result under a host fence before calling atomic storage.
// It does not deliver public content or expose unauthenticated historical reads.
// An error can follow a durable commit, for example if the host loses its final
// acknowledgement. Reconcile with the same request and ResultID; never mint a
// new ID to hide an ambiguous outcome.
type Committer interface {
	Commit(context.Context, CommitRequest) (CommitReceipt, error)
}

type guardedCommitter struct {
	storage    CommitStore
	fence      CommitFence
	authorizer BindingAuthorizer
}

// NewCommitter requires explicit storage, fencing and current grant checks.
// The authorizer runs inside the fence, including on idempotent retries.
func NewCommitter(storage CommitStore, fence CommitFence, authorizer BindingAuthorizer) (Committer, error) {
	if nilCommitPort(storage) || nilCommitPort(fence) || nilCommitPort(authorizer) {
		return nil, ErrInvalid
	}
	return &guardedCommitter{storage: storage, fence: fence, authorizer: authorizer}, nil
}

func nilCommitPort(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

func (c *guardedCommitter) Commit(ctx context.Context, request CommitRequest) (CommitReceipt, error) {
	if err := ctx.Err(); err != nil {
		return CommitReceipt{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return CommitReceipt{}, err
	}
	frozen, digest, err := request.Freeze()
	if err != nil {
		return CommitReceipt{}, err
	}
	if frozen.Current.Scope.Canonical() != scope.Canonical() {
		return CommitReceipt{}, ErrInvalid
	}
	expected, err := cloneBinding(frozen.Current)
	if err != nil {
		return CommitReceipt{}, err
	}
	attemptCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// The fence is trusted host code. Reject missing, repeated or retained calls,
	// and cancel escaped work if an adapter returns before its callback finishes.
	var mu sync.Mutex
	active, started, finished := true, false, false
	var receipt CommitReceipt
	var callbackErr, protocolErr error
	fenceErr := c.fence.WithCommitFence(attemptCtx, expected, func(current Binding) error {
		mu.Lock()
		if !active || started {
			protocolErr = ErrInvalid
			mu.Unlock()
			return ErrInvalid
		}
		started = true
		mu.Unlock()
		result, commitErr := c.commitUnderFence(attemptCtx, frozen, digest, current)
		mu.Lock()
		if !active {
			result = CommitReceipt{}
			if commitErr == nil {
				commitErr = ErrInvalid
			}
		}
		receipt, callbackErr, finished = result, commitErr, true
		mu.Unlock()
		return commitErr
	})
	mu.Lock()
	active = false
	done := started && finished
	result, resultErr, invalid := receipt, callbackErr, protocolErr
	mu.Unlock()
	cancel()
	if fenceErr != nil {
		return CommitReceipt{}, fmt.Errorf("cortex: result commit fence: %w", fenceErr)
	}
	if invalid != nil {
		return CommitReceipt{}, invalid
	}
	if !done {
		return CommitReceipt{}, ErrInvalid
	}
	if resultErr != nil {
		return CommitReceipt{}, resultErr
	}
	if err := ctx.Err(); err != nil {
		return CommitReceipt{}, err
	}
	return result, nil
}

func (c *guardedCommitter) commitUnderFence(ctx context.Context, req CommitRequest, digest string, current Binding) (CommitReceipt, error) {
	if err := ctx.Err(); err != nil {
		return CommitReceipt{}, err
	}
	currentDigest, err := current.Digest()
	if err != nil {
		return CommitReceipt{}, err
	}
	expectedDigest, err := req.Current.Digest()
	if err != nil {
		return CommitReceipt{}, err
	}
	if currentDigest != expectedDigest {
		return CommitReceipt{}, ErrConflict
	}
	authorized, err := cloneBinding(current)
	if err != nil {
		return CommitReceipt{}, err
	}
	if err = c.authorizer.AuthorizeVerification(ctx, authorized); err != nil {
		return CommitReceipt{}, fmt.Errorf("cortex: authorize result commit: %w", err)
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return CommitReceipt{}, contextErr
	}
	receipt, err := c.storage.CommitVerifiedResult(ctx, req)
	if err != nil {
		return CommitReceipt{}, err
	}
	if validationErr := receipt.Validate(); validationErr != nil {
		return CommitReceipt{}, validationErr
	}
	if receipt.RequestDigest != digest {
		return CommitReceipt{}, ErrConflict
	}
	// Detach storage-owned slices before returning through host code.
	return SealCommitReceipt(receipt)
}
