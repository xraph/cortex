package artifacts

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
)

// RetentionReader reads exact-scope artifact metadata without returning bytes.
type RetentionReader interface {
	GetArtifact(context.Context, optimization.ArtifactRef) (optimization.ArtifactRetention, error)
}

// RetainedReader checks both durable eligibility and freshly verified content.
// Its final check establishes an observation point, not a lease over returned
// bytes. Decision commits must still use transactional retention checks.
type RetainedReader struct {
	metadata RetentionReader
	verifier *optimization.ArtifactVerifier
	now      func() time.Time
}

// NewRetainedReader requires a provider that enforces current artifact grants.
// The maximum byte limit applies through the shared content verifier.
func NewRetainedReader(metadata RetentionReader, provider optimization.ArtifactReader, maxBytes int64, now func() time.Time) (*RetainedReader, error) {
	if metadata == nil {
		return nil, optimization.ErrInvalidSpec
	}
	verifier, err := optimization.NewArtifactVerifier(provider, maxBytes, now)
	if err != nil {
		return nil, err
	}
	return &RetainedReader{metadata: metadata, verifier: verifier, now: now}, nil
}

// Get refuses missing, expired or invalidated evidence, including deletion that
// commits while content is being fetched. Provider retention cannot override the
// dates registered in the metadata transaction.
func (r *RetainedReader) Get(ctx context.Context, ref optimization.ArtifactRef) (optimization.Artifact, error) {
	ref = detachRef(ref)
	before, err := r.retained(ctx, ref)
	if err != nil {
		return optimization.Artifact{}, err
	}
	value, err := r.verifier.Verify(ctx, ref.ExperimentID, ref)
	if err != nil {
		return optimization.Artifact{}, err
	}
	if !value.CreatedAt.Equal(before.CreatedAt) || !value.ExpiresAt.Equal(before.ExpiresAt) {
		return optimization.Artifact{}, optimization.ErrIncompleteEvidence
	}
	after, err := r.retained(ctx, ref)
	if err != nil {
		return optimization.Artifact{}, err
	}
	if before.Revision != after.Revision || !before.CreatedAt.Equal(after.CreatedAt) || !before.ExpiresAt.Equal(after.ExpiresAt) || !before.RegisteredAt.Equal(after.RegisteredAt) || before.RegisteredBy != after.RegisteredBy {
		return optimization.Artifact{}, optimization.ErrIncompleteEvidence
	}
	return value, nil
}

func (r *RetainedReader) retained(ctx context.Context, ref optimization.ArtifactRef) (optimization.ArtifactRetention, error) {
	var empty optimization.ArtifactRetention
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	binding, err := ref.BindingDigest()
	if err != nil {
		return empty, err
	}
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return empty, cortex.ErrNoScope
	}
	key, err := optimization.ScopeKey(scope)
	if err != nil {
		return empty, err
	}
	if key != ref.Scope.Canonical() {
		return empty, optimization.ErrPolicyDenied
	}
	value, err := r.metadata.GetArtifact(detachContext(ctx), detachRef(ref))
	if err != nil {
		return empty, fmt.Errorf("%w: artifact retention: %w", optimization.ErrIncompleteEvidence, err)
	}
	actual, err := value.Ref.BindingDigest()
	if err != nil {
		return empty, fmt.Errorf("%w: retention identity: %w", optimization.ErrIncompleteEvidence, err)
	}
	now := r.now()
	if actual != binding || value.Revision != 1 || value.Deletion != nil || !value.DeletedAt.IsZero() || !value.InvalidatedAt.IsZero() || value.CreatedAt.IsZero() || value.RegisteredAt.IsZero() || now.Before(value.CreatedAt) || now.Before(value.RegisteredAt) || !now.Before(value.ExpiresAt) {
		return empty, optimization.ErrIncompleteEvidence
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return value, nil
}
