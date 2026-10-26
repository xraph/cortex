package operator_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
)

func TestFailureCodes(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
		code   string
	}{
		{opt.ErrInvalidSpec, 400, "optimization_invalid_request"},
		{cortex.ErrNoScope, 403, "optimization_denied"},
		{opt.ErrPolicyDenied, 403, "optimization_denied"},
		{opt.ErrNotFound, 404, "optimization_not_found"},
		{opt.ErrConflict, 409, "optimization_conflict"},
		{opt.ErrStaleVersion, 409, "optimization_stale_version"},
		{opt.ErrBudgetExceeded, 409, "optimization_budget_exceeded"},
		{opt.ErrUnknownAttempt, 409, "optimization_unknown_attempt"},
		{opt.ErrIncompleteEvidence, 409, "optimization_incomplete_evidence"},
		{opt.ErrInactiveActivation, 409, "optimization_inactive"},
		{opt.ErrUnavailable, 503, "optimization_unavailable"},
		{opt.ErrNoEnvironment, 503, "optimization_no_environment"},
		{context.DeadlineExceeded, 504, "optimization_timeout"},
		{context.Canceled, 408, "optimization_canceled"},
		{errors.New("SECRET storage"), 503, "optimization_unavailable"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			failure := operator.Failure(fmt.Errorf("SECRET backend: %w", tc.err))
			if failure.Status != tc.status || failure.Code != tc.code || strings.Contains(failure.Message, "SECRET") {
				t.Fatalf("failure: %+v", failure)
			}
		})
	}
}
