// Package evidence defines immutable, scoped artifacts used by result checks.
package evidence

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// PrefixArtifact identifies immutable evidence objects.
const PrefixArtifact id.Prefix = "evi"

// Sensitivity is a host-enforced classification, never an access grant.
type Sensitivity string

const (
	SensitivityPublic     Sensitivity = "public"
	SensitivityInternal   Sensitivity = "internal"
	SensitivityRestricted Sensitivity = "restricted"
)

// Ref binds content and its identity. SourceURI is an opaque host resource
// reference, not a download URL. It must not contain credentials.
type Ref struct {
	SchemaVersion int          `json:"schema_version"`
	ID            id.ID        `json:"id"`
	SHA256        string       `json:"sha256"`
	MediaType     string       `json:"media_type"`
	Bytes         int64        `json:"bytes"`
	Scope         cortex.Scope `json:"scope"`
	Version       string       `json:"version"`
	Producer      string       `json:"producer"`
	SourceURI     string       `json:"source_uri,omitempty"`
	Sensitivity   Sensitivity  `json:"sensitivity"`
	CreatedAt     time.Time    `json:"created_at"`
	RetainUntil   time.Time    `json:"retain_until"`
}

// Artifact is the bounded content supplied by a trusted capture adapter.
type Artifact struct {
	Ref  Ref    `json:"ref"`
	Data []byte `json:"data"`
}

// Seal detaches content and assigns its digest and byte count. The host supplies
// identity, scope and timestamps; model output must not supply those fields.
func Seal(a Artifact) (Artifact, error) {
	a.Data = append([]byte(nil), a.Data...)
	a.Ref.Scope.Levels = append([]cortex.Level(nil), a.Ref.Scope.Levels...)
	a.Ref.CreatedAt = a.Ref.CreatedAt.UTC()
	a.Ref.RetainUntil = a.Ref.RetainUntil.UTC()
	a.Ref.SHA256 = Digest(a.Data)
	a.Ref.Bytes = int64(len(a.Data))
	return a, a.Validate()
}

// Validate checks reference structure without granting access to its content.
func (r Ref) Validate() error {
	for _, value := range []string{r.MediaType, r.Version, r.Producer, r.SourceURI, string(r.Sensitivity), r.Scope.Canonical()} {
		if !utf8.ValidString(value) {
			return ErrInvalid
		}
	}
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != PrefixArtifact || !ValidDigest(r.SHA256) || r.Bytes < 0 || strings.TrimSpace(r.Version) == "" || strings.TrimSpace(r.MediaType) == "" || strings.TrimSpace(r.Producer) == "" || r.CreatedAt.IsZero() || !r.RetainUntil.After(r.CreatedAt) {
		return ErrInvalid
	}
	if err := cortex.ValidateScope(r.Scope); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	switch r.Sensitivity {
	case SensitivityPublic, SensitivityInternal, SensitivityRestricted:
	default:
		return ErrInvalid
	}
	return nil
}

// Validate rejects content that no longer matches its immutable reference.
func (a Artifact) Validate() error {
	if err := a.Ref.Validate(); err != nil {
		return err
	}
	if a.Ref.Bytes != int64(len(a.Data)) || a.Ref.SHA256 != Digest(a.Data) {
		return ErrDigestMismatch
	}
	return nil
}
