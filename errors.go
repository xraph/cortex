package cortex

import "errors"

var (
	// Store errors.
	ErrNoStore         = errors.New("cortex: no store configured")
	ErrStoreClosed     = errors.New("cortex: store closed")
	ErrMigrationFailed = errors.New("cortex: migration failed")

	// Not found errors.
	ErrAgentNotFound      = errors.New("cortex: agent not found")
	ErrRunNotFound        = errors.New("cortex: run not found")
	ErrStepNotFound       = errors.New("cortex: step not found")
	ErrToolCallNotFound   = errors.New("cortex: tool call not found")
	ErrSkillNotFound      = errors.New("cortex: skill not found")
	ErrTraitNotFound      = errors.New("cortex: trait not found")
	ErrBehaviorNotFound   = errors.New("cortex: behavior not found")
	ErrPersonaNotFound    = errors.New("cortex: persona not found")
	ErrCheckpointNotFound = errors.New("cortex: checkpoint not found")

	// Conflict errors.
	ErrAlreadyExists = errors.New("cortex: resource already exists")

	// State errors.
	ErrInvalidState     = errors.New("cortex: invalid state transition")
	ErrRunCancelled     = errors.New("cortex: run cancelled")
	ErrRunAlreadyDone   = errors.New("cortex: run already completed")
	ErrBudgetExhausted  = errors.New("cortex: budget exhausted")
	ErrMaxStepsReached  = errors.New("cortex: maximum steps reached")
	ErrMaxTokensReached = errors.New("cortex: maximum tokens reached")
)
