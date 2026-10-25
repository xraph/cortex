package orchestration

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
)

func TestOnlyWaitingPreservesParallelFailures(t *testing.T) {
	waiting := cortex.ErrOrchestrationWaiting
	require.True(t, IsWaiting(waiting))
	require.True(t, IsWaiting(errors.Join(waiting, fmt.Errorf("worker: %w", waiting))))
	require.False(t, IsWaiting(errors.Join(waiting, cortex.ErrRunIncomplete)))
	require.False(t, IsWaiting(errors.Join(waiting, errors.New("provider failed"))))
	require.False(t, IsWaiting(nil))
}
