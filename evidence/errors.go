package evidence

import "github.com/xraph/cortex"

// Error aliases preserve the shared Cortex sentinel identities.
var (
	ErrInvalid        = cortex.ErrInvalidEvidence
	ErrDigestMismatch = cortex.ErrEvidenceDigestMismatch
	ErrDenied         = cortex.ErrEvidenceAccessDenied
	ErrTooLarge       = cortex.ErrEvidenceTooLarge
	ErrNotFound       = cortex.ErrEvidenceNotFound
	ErrDeleted        = cortex.ErrEvidenceDeleted
	ErrConflict       = cortex.ErrEvidenceConflict
)
