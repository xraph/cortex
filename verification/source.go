package verification

import (
	"strings"

	"github.com/xraph/cortex/evidence"
)

// Validate checks a source reference without establishing its authenticity or
// present availability. The host supplies the captured revision and timestamp.
func (s Source) Validate() error {
	if !validText(s.Resource, s.Revision) || strings.TrimSpace(s.Resource) == "" || strings.TrimSpace(s.Revision) == "" || !evidence.ValidDigest(s.Digest) || s.RetrievedAt.IsZero() {
		return ErrInvalid
	}
	return nil
}

// ValidateSources checks bounded, unique resource provenance.
func ValidateSources(sources []Source) error {
	if len(sources) > 256 {
		return ErrInvalid
	}
	seen := make(map[string]bool, len(sources))
	for _, source := range sources {
		if source.Validate() != nil || seen[source.Resource] {
			return ErrInvalid
		}
		seen[source.Resource] = true
	}
	return nil
}

// Equal compares captured provenance independently of clock representations.
func (s Source) Equal(other Source) bool {
	return s.Resource == other.Resource && s.Revision == other.Revision && s.Digest == other.Digest && s.RetrievedAt.Equal(other.RetrievedAt)
}
