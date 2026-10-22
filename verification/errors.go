package verification

import "github.com/xraph/cortex"

// Error aliases preserve the shared Cortex sentinel identities.
var (
	ErrInvalid  = cortex.ErrInvalidVerification
	ErrConflict = cortex.ErrVerificationConflict
	ErrNotFound = cortex.ErrVerificationNotFound
)
