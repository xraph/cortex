// Package artifacts coordinates protected content with optimization retention.
package artifacts

import (
	"context"
	"fmt"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

// DeletionMetadata is the transactional metadata required for deletion recovery.
type DeletionMetadata interface {
	GetArtifact(context.Context, optimization.ArtifactRef) (optimization.ArtifactRetention, error)
	GetExperiment(context.Context, id.ID) (optimization.Experiment, error)
	Transact(context.Context, func(optimization.Transaction) error) error
}

// DeletionReceipt identifies a durable tombstone verified by the content adapter.
// A missing object or successful transport response alone is not a receipt.
type DeletionReceipt struct {
	BindingDigest string
	RequestID     string
	Actor         optimization.IdentityRef
	Reason        string
}

// Tombstoner must check current provider grants and persist a permanent fence
// against delayed uploads, including for an absent object. Identical requests are
// idempotent. Return a receipt only after verifying the stored tombstone's exact
// reference, request, actor and reason. Unknown outcomes must remain errors.
type Tombstoner interface {
	Tombstone(context.Context, optimization.ArtifactDeletion) (DeletionReceipt, error)
}

// DeletionService invalidates evidence before attempting physical deletion.
// It makes one provider call per invocation. Pending requests survive crashes and
// can be retried through Recover with fresh grants for the saved original actor.
type DeletionService struct {
	metadata   DeletionMetadata
	provider   Tombstoner
	authorizer optimization.Authorizer
	resolver   optimization.IdentityResolver
}

// NewDeletionService requires explicit storage, provider and host authority.
func NewDeletionService(metadata DeletionMetadata, provider Tombstoner, authorizer optimization.Authorizer, resolver optimization.IdentityResolver) (*DeletionService, error) {
	if metadata == nil || provider == nil || authorizer == nil || resolver == nil {
		return nil, optimization.ErrInvalidSpec
	}
	return &DeletionService{metadata: metadata, provider: provider, authorizer: authorizer, resolver: resolver}, nil
}

// Delete accepts a host-attributed command. The host must bind Actor to the
// authenticated caller, never accept a model-supplied or untrusted subject.
func (s *DeletionService) Delete(ctx context.Context, request optimization.ArtifactDeletion) error {
	request.Ref = detachRef(request.Ref)
	if !optimization.ValidOpaqueKey(request.RequestID) || !optimization.ValidOpaqueKey(request.Actor.Subject) || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 4096 {
		return optimization.ErrInvalidSpec
	}
	if err := s.authorize(ctx, request.Ref); err != nil {
		return err
	}
	resolved, cleanup, err := s.resolve(ctx, request)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := s.authorize(resolved, request.Ref); err != nil {
		return err
	}
	if err := s.metadata.Transact(resolved, func(tx optimization.Transaction) error { return tx.BeginArtifactDeletion(request) }); err != nil {
		return err
	}
	return s.finish(resolved, request.Ref)
}

// Recover resumes only a previously saved deletion. It never creates a request
// or substitutes the recovering worker's identity for the original actor.
func (s *DeletionService) Recover(ctx context.Context, ref optimization.ArtifactRef) error {
	ref = detachRef(ref)
	if err := s.authorize(ctx, ref); err != nil {
		return err
	}
	value, err := s.metadata.GetArtifact(ctx, ref)
	if err != nil {
		return err
	}
	if value.Deletion == nil {
		return optimization.ErrConflict
	}
	resolved, cleanup, err := s.resolve(ctx, *value.Deletion)
	if err != nil {
		return err
	}
	defer cleanup()
	return s.finish(resolved, ref)
}

func (s *DeletionService) finish(ctx context.Context, ref optimization.ArtifactRef) error {
	if err := s.authorize(ctx, ref); err != nil {
		return err
	}
	value, err := s.metadata.GetArtifact(ctx, ref)
	if err != nil {
		return err
	}
	if value.Deletion == nil {
		return optimization.ErrConflict
	}
	request := *value.Deletion
	binding, err := ref.BindingDigest()
	if err != nil {
		return err
	}
	savedBinding, err := request.Ref.BindingDigest()
	if err != nil || binding != savedBinding {
		return optimization.ErrIncompleteEvidence
	}
	if !value.DeletedAt.IsZero() {
		return nil
	}
	request.Ref = detachRef(request.Ref)
	proof, err := s.provider.Tombstone(detachContext(ctx), request)
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	if err != nil {
		return fmt.Errorf("cortex: optimization: tombstone artifact: %w", err)
	}
	if proof.BindingDigest != binding || proof.RequestID != request.RequestID || proof.Actor != request.Actor || proof.Reason != request.Reason {
		return optimization.ErrIncompleteEvidence
	}
	if err := s.authorize(ctx, ref); err != nil {
		return err
	}
	return s.metadata.Transact(ctx, func(tx optimization.Transaction) error { return tx.CompleteArtifactDeletion(ref, request.RequestID) })
}

func (s *DeletionService) authorize(ctx context.Context, ref optimization.ArtifactRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ref.Validate(); err != nil {
		return err
	}
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return cortex.ErrNoScope
	}
	key, err := optimization.ScopeKey(scope)
	if err != nil {
		return err
	}
	if key != ref.Scope.Canonical() {
		return optimization.ErrPolicyDenied
	}
	experiment, err := s.metadata.GetExperiment(ctx, ref.ExperimentID)
	if err != nil {
		return err
	}
	resource := optimization.Resource{Scope: detachRef(ref).Scope, AgentID: experiment.Spec.AgentID, ID: ref.ExperimentID, Kind: "artifact"}
	if err := s.authorizer.Authorize(detachContext(ctx), optimization.ActionDelete, resource); err != nil {
		return err
	}
	return ctx.Err()
}

func (s *DeletionService) resolve(ctx context.Context, request optimization.ArtifactDeletion) (context.Context, context.CancelFunc, error) {
	return optimization.ResolveIdentity(ctx, s.resolver, request.Actor, request.Ref.Scope)
}

func detachRef(ref optimization.ArtifactRef) optimization.ArtifactRef {
	ref.Scope.Levels = append([]cortex.Level(nil), ref.Scope.Levels...)
	return ref
}

func detachContext(ctx context.Context) context.Context {
	scope := cortex.ScopeFromContext(ctx)
	scope.Levels = append([]cortex.Level(nil), scope.Levels...)
	return cortex.WithScope(ctx, scope)
}
