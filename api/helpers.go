package api

import (
	"errors"
	"net/http"

	"github.com/xraph/forge"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
)

// mapStoreError maps domain errors to Forge HTTP errors.
func mapStoreError(err error) error {
	if err == nil {
		return nil
	}
	if failure, ok := run.DescribeVerificationFailure(err, nil); ok {
		return verificationHTTPError{failure: failure}
	}
	switch {
	case errors.Is(err, cortex.ErrRunConflict):
		return forge.NewHTTPError(http.StatusConflict, cortex.ErrRunConflict.Error())
	case errors.Is(err, cortex.ErrPreparationUnavailable):
		return forge.NewHTTPError(http.StatusServiceUnavailable, cortex.ErrPreparationUnavailable.Error())
	case errors.Is(err, safety.ErrUnavailable):
		return forge.NewHTTPError(http.StatusServiceUnavailable, safety.ErrUnavailable.Error())
	case errors.Is(err, safety.ErrBlocked):
		return forge.NewHTTPError(http.StatusForbidden, safety.ErrBlocked.Error())
	case errors.Is(err, safety.ErrLimit):
		return forge.NewHTTPError(http.StatusRequestEntityTooLarge, safety.ErrLimit.Error())
	}
	if isNotFound(err) {
		return forge.NotFound(err.Error())
	}
	if isConflict(err) {
		return forge.NewHTTPError(http.StatusConflict, err.Error())
	}
	if errors.Is(err, cortex.ErrInvalidScope) || errors.Is(err, cortex.ErrUnsupportedScopeDepth) {
		return forge.NewHTTPError(http.StatusForbidden, "cortex: request scope is invalid or unsupported")
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

// notFoundErrors are the cortex sentinels the module answers with 404.
var notFoundErrors = []error{
	cortex.ErrAgentNotFound,
	cortex.ErrSkillNotFound,
	cortex.ErrTraitNotFound,
	cortex.ErrBehaviorNotFound,
	cortex.ErrPersonaNotFound,
	cortex.ErrRunNotFound,
	cortex.ErrCheckpointNotFound,
	cortex.ErrOrchestrationNotFound,
	cortex.ErrOrchestrationRunNotFound,
	cortex.ErrSessionNotFound,
}

func isNotFound(err error) bool {
	return notFoundSentinel(err) != nil
}

// notFoundSentinel is the not-found sentinel err wraps, or nil.
func notFoundSentinel(err error) error {
	for _, sentinel := range notFoundErrors {
		if errors.Is(err, sentinel) {
			return sentinel
		}
	}
	return nil
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
