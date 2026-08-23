package api

import (
	"errors"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex"
)

// mapStoreError maps domain errors to Forge HTTP errors.
func mapStoreError(err error) error {
	if err == nil {
		return nil
	}
	if isNotFound(err) {
		return forge.NotFound(err.Error())
	}
	if isConflict(err) {
		return forge.NewHTTPError(http.StatusConflict, err.Error())
	}
	if errors.Is(err, cortex.ErrNoScope) {
		// The host never attached a scope to the request context. That is
		// a caller/configuration fault (a missing precondition on every
		// scope-guarded call), not a server fault, so it must not read as
		// a 500.
		return forge.NewHTTPError(http.StatusPreconditionFailed,
			"cortex: request has no scope; the host must attach one with cortex.WithScope")
	}
	return err
}

func isNotFound(err error) bool {
	return errors.Is(err, cortex.ErrAgentNotFound) ||
		errors.Is(err, cortex.ErrSkillNotFound) ||
		errors.Is(err, cortex.ErrTraitNotFound) ||
		errors.Is(err, cortex.ErrBehaviorNotFound) ||
		errors.Is(err, cortex.ErrPersonaNotFound) ||
		errors.Is(err, cortex.ErrRunNotFound) ||
		errors.Is(err, cortex.ErrCheckpointNotFound) ||
		errors.Is(err, cortex.ErrOrchestrationNotFound) ||
		errors.Is(err, cortex.ErrOrchestrationRunNotFound) ||
		errors.Is(err, cortex.ErrSessionNotFound)
}

func isConflict(err error) bool {
	return errors.Is(err, cortex.ErrAlreadyExists)
}

// defaultLimit returns a safe default page size.
func defaultLimit(limit int) int {
	if limit <= 0 {
		return 50
	}
	if limit > 1000 {
		return 1000
	}
	return limit
}
