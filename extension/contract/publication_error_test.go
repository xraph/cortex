package contract

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	dc "github.com/xraph/forge/extensions/dashboard/contract"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/safety"
)

func TestPublicationErrorsRemainDistinctWithoutCandidateText(t *testing.T) {
	for _, tc := range []struct {
		cause error
		code  dc.ErrorCode
		retry bool
	}{{cortex.ErrMaxStepsReached, dc.CodeConflict, false}, {cortex.ErrBudgetExhausted, dc.CodeConflict, false}, {safety.ErrBlocked, dc.CodePermissionDenied, false}, {safety.ErrLimit, dc.CodeBadRequest, false}, {safety.ErrUnavailable, dc.CodeUnavailable, true}} {
		mapped := mapError(fmt.Errorf("private candidate: %w", tc.cause))
		var result *dc.Error
		if !errors.As(mapped, &result) || result.Code != tc.code || result.Retryable != tc.retry {
			t.Fatalf("wrong publication classification for %v: %+v", tc.cause, result)
		}
		if strings.Contains(result.Message, "private candidate") {
			t.Fatal("candidate leaked")
		}
	}
}
