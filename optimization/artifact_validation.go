package optimization

import (
	"fmt"
	"mime"
	"strings"

	"github.com/xraph/cortex/optimization/internal/canonical"
)

// Validate checks the complete protected identity, without claiming availability.
func (ref ArtifactRef) Validate() error {
	if _, err := ParseID(ref.ExperimentID.String(), PrefixExperiment); err != nil {
		return err
	}
	if _, err := ScopeKey(ref.Scope); err != nil {
		return err
	}
	if !validDigest(ref.Digest) || ref.Size < 0 || !ValidOpaqueKey(ref.AccessPolicy) || !ValidOpaqueKey(ref.Kind) || !ValidOpaqueKey(ref.MediaType) {
		return fmt.Errorf("%w: malformed artifact reference", ErrInvalidSpec)
	}
	mediaType, _, err := mime.ParseMediaType(ref.MediaType)
	if err != nil || !strings.Contains(mediaType, "/") {
		return fmt.Errorf("%w: invalid artifact media type", ErrInvalidSpec)
	}
	return nil
}

// BindingDigest binds content identity to its scope, media type, kind and policy.
// Equal byte digests alone do not make two protected references interchangeable.
func (ref ArtifactRef) BindingDigest() (string, error) {
	if err := ref.Validate(); err != nil {
		return "", err
	}
	encoded, err := canonical.Marshal(ref)
	if err != nil {
		return "", fmt.Errorf("%w: encode artifact reference: %w", ErrInvalidSpec, err)
	}
	return digestBytes(encoded), nil
}

// Validate checks immutable content and a finite retention interval. Whether the
// artifact is currently retained and authorized requires a fresh provider read.
func (artifact Artifact) Validate() error {
	if err := artifact.Ref.Validate(); err != nil {
		return err
	}
	if artifact.CreatedAt.IsZero() || artifact.ExpiresAt.IsZero() || !artifact.ExpiresAt.After(artifact.CreatedAt) || artifact.Ref.Size != int64(len(artifact.Data)) || artifact.Ref.Digest != digestBytes(artifact.Data) {
		return fmt.Errorf("%w: invalid artifact content or retention", ErrInvalidSpec)
	}
	return nil
}
