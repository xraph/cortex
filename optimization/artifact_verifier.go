package optimization

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// ArtifactReader must enforce current host grants and exact scope on every read.
// Implementations must bound allocations before fetching content. The verifier
// checks returned bytes, but cannot control allocations inside an injected reader.
type ArtifactReader interface {
	Get(context.Context, ArtifactRef) (Artifact, error)
}

// ArtifactVerifier checks freshly retrieved evidence before a service decision.
// Verification is a point-in-time observation, not a retention lease or an atomic
// decision fence. Services must also coordinate deletion with persisted eligibility.
type ArtifactVerifier struct {
	reader   ArtifactReader
	maxBytes int64
	now      func() time.Time
}

// NewArtifactVerifier requires an authorized reader, a byte limit and a clock.
func NewArtifactVerifier(reader ArtifactReader, maxBytes int64, now func() time.Time) (*ArtifactVerifier, error) {
	if reader == nil || maxBytes <= 0 || now == nil {
		return nil, ErrInvalidSpec
	}
	return &ArtifactVerifier{reader: reader, maxBytes: maxBytes, now: now}, nil
}

// Verify returns detached content only when its complete reference, experiment,
// digest and retention interval match. Provider refusals remain inspectable while
// also marking the required evidence incomplete.
func (v *ArtifactVerifier) Verify(ctx context.Context, experimentID id.ID, ref ArtifactRef) (Artifact, error) {
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	if _, err := ParseID(experimentID.String(), PrefixExperiment); err != nil {
		return Artifact{}, err
	}
	if ref.ExperimentID != experimentID {
		return Artifact{}, fmt.Errorf("%w: artifact belongs to another experiment", ErrIncompleteEvidence)
	}
	expected, err := ref.BindingDigest()
	if err != nil {
		return Artifact{}, err
	}
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return Artifact{}, cortex.ErrNoScope
	}
	key, err := ScopeKey(scope)
	if err != nil {
		return Artifact{}, err
	}
	if key != ref.Scope.Canonical() {
		return Artifact{}, ErrPolicyDenied
	}
	if ref.Size > v.maxBytes {
		return Artifact{}, fmt.Errorf("%w: artifact exceeds configured byte limit", ErrIncompleteEvidence)
	}
	ref.Scope.Levels = append([]cortex.Level(nil), ref.Scope.Levels...)
	scope.Levels = append([]cortex.Level(nil), scope.Levels...)
	artifact, err := v.reader.Get(cortex.WithScope(ctx, scope), ref)
	if contextErr := ctx.Err(); contextErr != nil {
		return Artifact{}, contextErr
	}
	if err != nil {
		return Artifact{}, fmt.Errorf("%w: read artifact: %w", ErrIncompleteEvidence, err)
	}
	if int64(len(artifact.Data)) > v.maxBytes {
		return Artifact{}, fmt.Errorf("%w: returned artifact exceeds configured byte limit", ErrIncompleteEvidence)
	}
	artifact.Data = append([]byte(nil), artifact.Data...)
	artifact.Ref.Scope.Levels = append([]cortex.Level(nil), artifact.Ref.Scope.Levels...)
	if err = artifact.Validate(); err != nil {
		return Artifact{}, fmt.Errorf("%w: validate artifact: %w", ErrIncompleteEvidence, err)
	}
	actual, err := artifact.Ref.BindingDigest()
	if err != nil {
		return Artifact{}, fmt.Errorf("%w: bind artifact: %w", ErrIncompleteEvidence, err)
	}
	now := v.now()
	if actual != expected || now.IsZero() || now.Before(artifact.CreatedAt) || !now.Before(artifact.ExpiresAt) {
		return Artifact{}, fmt.Errorf("%w: artifact identity or retention mismatch", ErrIncompleteEvidence)
	}
	if err := ctx.Err(); err != nil {
		return Artifact{}, err
	}
	return artifact, nil
}
