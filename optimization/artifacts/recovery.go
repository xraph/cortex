package artifacts

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
)

// RecoveryMetadata discovers deletion work within one exact host scope.
type RecoveryMetadata interface {
	ListArtifactWork(context.Context, time.Time, optimization.PageQuery) (optimization.Page[optimization.ArtifactRetention], error)
	GetArtifact(context.Context, optimization.ArtifactRef) (optimization.ArtifactRetention, error)
}

// RecoveryOutcome records one attempted reconciliation, including its failure.
type RecoveryOutcome struct {
	Ref optimization.ArtifactRef
	Err error
}

// RecoveryResult advances only a fully visited page. Individual errors remain in
// Outcomes and must be retried by a later sweep starting with an empty cursor.
type RecoveryResult struct {
	Outcomes   []RecoveryOutcome
	NextCursor string
}

// Recovery processes a single page per call. The host owns scheduling, scope
// enumeration, continuation persistence and reporting failures. Providers must
// honor cancellation; no background goroutine is started to abandon a delete.
type Recovery struct {
	metadata RecoveryMetadata
	deletion *DeletionService
	actor    optimization.IdentityRef
	timeout  time.Duration
}

// NewRecovery requires the host's retention identity for new expiry requests.
// Pending requests retain their saved actor. A zero timeout selects 30 seconds;
// an explicit per-item timeout must be positive and at most 30 seconds.
func NewRecovery(metadata RecoveryMetadata, deletion *DeletionService, actor optimization.IdentityRef, timeout time.Duration) (*Recovery, error) {
	if metadata == nil || deletion == nil || !optimization.ValidOpaqueKey(actor.Subject) || timeout < 0 || timeout > 30*time.Second {
		return nil, optimization.ErrInvalidSpec
	}
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	return &Recovery{metadata: metadata, deletion: deletion, actor: actor, timeout: timeout}, nil
}

// RunPage uses one fixed cutoff throughout a sweep. A failed call must be retried
// with the same input cursor; successful item operations are idempotent. A
// successful call may still contain individual failures. Start a fresh sweep
// after reaching an empty NextCursor so failed or newly eligible rows are found.
// Page limits bound provider calls, not the store's current snapshot hydration.
func (r *Recovery) RunPage(ctx context.Context, cutoff time.Time, query optimization.PageQuery) (RecoveryResult, error) {
	result := RecoveryResult{Outcomes: []RecoveryOutcome{}}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return result, cortex.ErrNoScope
	}
	if _, err := optimization.ScopeKey(scope); err != nil {
		return result, err
	}
	if cutoff.IsZero() || query.Limit < 0 || query.Limit > 500 {
		return result, optimization.ErrInvalidSpec
	}
	limit := query.Limit
	if limit == 0 {
		limit = 100
	}
	page, err := r.metadata.ListArtifactWork(detachContext(ctx), cutoff, query)
	if contextErr := ctx.Err(); contextErr != nil {
		return result, contextErr
	}
	if err != nil {
		return result, err
	}
	if len(page.Items) > limit {
		return result, optimization.ErrIncompleteEvidence
	}
	for _, item := range page.Items {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		child, cancel := context.WithTimeout(ctx, r.timeout)
		itemErr := r.reconcile(child, item.Ref, cutoff)
		cancel()
		result.Outcomes = append(result.Outcomes, RecoveryOutcome{Ref: detachRef(item.Ref), Err: itemErr})
		if err := ctx.Err(); err != nil {
			return result, err
		}
	}
	result.NextCursor = page.NextCursor
	return result, nil
}

func (r *Recovery) reconcile(ctx context.Context, ref optimization.ArtifactRef, cutoff time.Time) error {
	binding, err := ref.BindingDigest()
	if err != nil {
		return err
	}
	if ref.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return optimization.ErrPolicyDenied
	}
	current, err := r.metadata.GetArtifact(detachContext(ctx), detachRef(ref))
	if err != nil {
		return err
	}
	currentBinding, err := current.Ref.BindingDigest()
	if err != nil || binding != currentBinding {
		return optimization.ErrIncompleteEvidence
	}
	if !current.DeletedAt.IsZero() {
		return nil
	}
	if current.Deletion != nil {
		return r.deletion.Recover(ctx, ref)
	}
	if current.ExpiresAt.IsZero() {
		return optimization.ErrIncompleteEvidence
	}
	if current.ExpiresAt.After(cutoff) {
		return nil
	}
	return r.deletion.Delete(ctx, optimization.ArtifactDeletion{Ref: ref, RequestID: "retention-" + binding, Actor: r.actor, Reason: "artifact retention elapsed"})
}
