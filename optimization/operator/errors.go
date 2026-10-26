package operator

import (
	"context"
	"errors"
	"net/http"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
)

// ErrorResponse contains public failure codes, never provider or storage text.
// Status is transport metadata and is omitted from response bodies.
type ErrorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Status  int    `json:"-"`
}

// Failure maps a non-nil service error to a stable public response. Unknown
// failures remain unavailable; callers must not infer that a command was undone.
func Failure(err error) ErrorResponse {
	switch {
	case errors.Is(err, opt.ErrInvalidSpec):
		return ErrorResponse{"optimization_invalid_request", "Invalid optimization request", http.StatusBadRequest}
	case errors.Is(err, opt.ErrPolicyDenied), errors.Is(err, cortex.ErrNoScope):
		return ErrorResponse{"optimization_denied", "Optimization access denied", http.StatusForbidden}
	case errors.Is(err, opt.ErrNotFound):
		return ErrorResponse{"optimization_not_found", "Optimization resource not found", http.StatusNotFound}
	case errors.Is(err, opt.ErrConflict):
		return ErrorResponse{"optimization_conflict", "Optimization revision or content conflicts", http.StatusConflict}
	case errors.Is(err, opt.ErrStaleVersion):
		return ErrorResponse{"optimization_stale_version", "Harness version requires review", http.StatusConflict}
	case errors.Is(err, opt.ErrBudgetExceeded):
		return ErrorResponse{"optimization_budget_exceeded", "Optimization budget exhausted", http.StatusConflict}
	case errors.Is(err, opt.ErrUnknownAttempt):
		return ErrorResponse{"optimization_unknown_attempt", "Original attempt outcome is unknown", http.StatusConflict}
	case errors.Is(err, opt.ErrIncompleteEvidence):
		return ErrorResponse{"optimization_incomplete_evidence", "Required optimization evidence is incomplete", http.StatusConflict}
	case errors.Is(err, opt.ErrInactiveActivation):
		return ErrorResponse{"optimization_inactive", "Optimization activation is disabled", http.StatusConflict}
	case errors.Is(err, opt.ErrNoEnvironment):
		return ErrorResponse{"optimization_no_environment", "Trial environment is unavailable", http.StatusServiceUnavailable}
	case errors.Is(err, context.DeadlineExceeded):
		return ErrorResponse{"optimization_timeout", "Optimization request timed out", http.StatusGatewayTimeout}
	case errors.Is(err, context.Canceled):
		return ErrorResponse{"optimization_canceled", "Optimization request was canceled", http.StatusRequestTimeout}
	default:
		return ErrorResponse{"optimization_unavailable", "Optimization capability is unavailable", http.StatusServiceUnavailable}
	}
}
