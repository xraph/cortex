package a2a

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/id"
)

func TestRoutingStoreRejectsUnqualifiedBackend(t *testing.T) {
	_, err := NewRoutingStore(newMemStore(), id.NewOrchestrationID())
	require.Error(t, err)
	_, err = NewRoutingStore(newMemStore(), id.NewAgentRunID())
	require.Error(t, err)
	_, err = NewRoutingStore(nil, id.Nil)
	require.Error(t, err)
}
