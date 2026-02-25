package api

import (
	"errors"

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
		return forge.NewHTTPError(409, err.Error())
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
		errors.Is(err, cortex.ErrCheckpointNotFound)
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
