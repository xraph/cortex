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
	require.True(t, onlyWaiting(waiting))
	require.True(t, onlyWaiting(errors.Join(waiting, fmt.Errorf("worker: %w", waiting))))
	require.False(t, onlyWaiting(errors.Join(waiting, cortex.ErrRunIncomplete)))
	require.False(t, onlyWaiting(errors.Join(waiting, errors.New("provider failed"))))
	require.False(t, onlyWaiting(nil))
}
