package optimization

import "errors"

// Stable errors let adapters distinguish refusal, conflict and unavailable evidence.
var (
	ErrInactiveActivation = errors.New("cortex: optimization: activation is disabled")
	ErrInvalidSpec        = errors.New("cortex: optimization: invalid specification")
	ErrPolicyDenied       = errors.New("cortex: optimization: edit denied by policy")
	ErrConflict           = errors.New("cortex: optimization: conflicting revision or content")
	ErrNoEnvironment      = errors.New("cortex: optimization: no trial environment")
	ErrUnavailable        = errors.New("cortex: optimization: capability unavailable")
	ErrBudgetExceeded     = errors.New("cortex: optimization: budget exceeded")
	ErrUnknownAttempt     = errors.New("cortex: optimization: attempt outcome unknown")
	ErrStaleVersion       = errors.New("cortex: optimization: stale version")
	ErrIncompleteEvidence = errors.New("cortex: optimization: incomplete evidence")
	ErrNotFound           = errors.New("cortex: optimization: resource not found")
)
