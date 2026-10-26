package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func TestOrchestrationPendingVerificationPreservesOtherFailures(t *testing.T) {
	child := &run.Run{ID: id.NewAgentRunID(), State: run.StateCompleted, Output: "private candidate", Provenance: run.ExecutionProvenance{VerificationRequired: true}, VerificationStatus: verification.StatusPending}
	for _, test := range []struct {
		cause   error
		waiting bool
	}{{nil, true}, {cortex.ErrVerificationUnavailable, true}, {cortex.ErrRunUnverified, true}, {errors.Join(cortex.ErrVerificationUnavailable, execution.ErrBudgetExceeded), false}, {context.Canceled, false}} {
		result, err := orchestrationAgentResult("worker", child, test.cause)
		require.NotNil(t, result)
		require.Empty(t, result.Output)
		if test.waiting {
			require.True(t, cortex.IsOrchestrationWaiting(err))
		} else {
			require.False(t, cortex.IsOrchestrationWaiting(err))
			require.ErrorIs(t, err, test.cause)
		}
	}
}
