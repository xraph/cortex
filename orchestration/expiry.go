package orchestration

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// BudgetExpiryStore seals admission only after the stored immutable deadline.
// True includes an already closed expired account. Errors grant no cleanup
// authority, including lost acknowledgments. Unknown usage remains reserved.
type BudgetExpiryStore interface {
	ExpireOrchestrationBudget(context.Context, id.OrchestrationID) (bool, error)
}

// BudgetListRequest bounds exact-scope recovery discovery by immutable identity.
type BudgetListRequest struct {
	After id.OrchestrationID
	Limit int
}

// BudgetDiscoveryStore includes closed accounts so interrupted parent cleanup
// remains discoverable. Listing a budget never grants execution authority.
type BudgetDiscoveryStore interface {
	ListOrchestrationBudgets(context.Context, BudgetListRequest) ([]BudgetPolicy, error)
}

// ExpiryStore combines atomic budget expiry with fenced parent updates.
type ExpiryStore interface {
	BudgetExpiryStore
	GetOrchestrationRun(context.Context, id.OrchestrationID) (*Run, error)
	UpdateOrchestrationRun(context.Context, *Run) error
}

// Expirer closes expired admission before finalizing prepared parent records.
// It never executes or resumes children, settles usage, or declares effects absent.
type Expirer struct{ store ExpiryStore }

// NewExpirer validates the explicitly supplied storage capability.
func NewExpirer(store ExpiryStore) (*Expirer, error) {
	if hostport.IsNil(store) {
		return nil, cortex.ErrOrchestrationContinuationUnavailable
	}
	return &Expirer{store: store}, nil
}

// Expire is safe to repeat after any interrupted write. The store checks time
// under the budget transaction. A stale parent snapshot fails its generation
// fence; retry from a fresh read. Existing terminal output remains unchanged.
func (e *Expirer) Expire(ctx context.Context, team id.OrchestrationID) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if team.IsNil() || team.Prefix() != id.PrefixOrchestration {
		return cortex.ErrInvalidContinuation
	}
	expired, err := e.store.ExpireOrchestrationBudget(ctx, team)
	if err != nil || !expired {
		return err
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	parent, err := e.store.GetOrchestrationRun(ctx, team)
	if errors.Is(err, cortex.ErrOrchestrationRunNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if parent == nil || parent.ID != team || parent.Scope.Canonical() != scope.Canonical() {
		return cortex.ErrInvalidContinuation
	}
	if parent.ContinuationGeneration == 0 || parent.Status == StatusCompleted || parent.Status == StatusFailed {
		return nil
	}
	if parent.ContinuationGeneration < 0 || parent.Status != StatusRunning && parent.Status != StatusWaiting {
		return cortex.ErrInvalidContinuation
	}
	detached := *parent
	parent = &detached
	completed := time.Now().UTC()
	parent.Status = StatusFailed
	parent.Error = "cortex: orchestration deadline exceeded; retained child outcomes require reconciliation"
	parent.Output = ""
	parent.CompletedAt = &completed
	return e.store.UpdateOrchestrationRun(ctx, parent)
}
