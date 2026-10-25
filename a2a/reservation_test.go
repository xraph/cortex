package a2a

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

type unavailableReservation struct{}

func (unavailableReservation) ReserveAgent(context.Context, id.OrchestrationID, string, string, *cortex.RunOpts) (*cortex.ReservedInvocation, error) {
	return nil, cortex.ErrRunIncomplete
}

func TestReservedBusRequiresPartitionAndBinding(t *testing.T) {
	_, err := NewBus(BusConfig{Store: newMemStore(), ReservedRunner: unavailableReservation{}})
	require.ErrorIs(t, err, cortex.ErrInvalidPrepared)
	_, err = NewBus(BusConfig{Store: newMemStore(), ReservedRunner: unavailableReservation{}, Runner: newFakeRunner(), OrchestrationID: id.NewOrchestrationID()})
	require.ErrorIs(t, err, cortex.ErrInvalidPrepared)
	_, err = NewBus(BusConfig{Store: newMemStore(), ReservedRunner: unavailableReservation{}, OrchestrationID: id.NewOrchestrationID()})
	require.ErrorIs(t, err, ErrDeliveryBindingUnavailable)
	wrapped, err := NewRoutingStore(newMemStore(), id.Nil)
	require.NoError(t, err)
	binder, ok := wrapped.(DeliveryRunBinder)
	require.True(t, ok)
	require.ErrorIs(t, binder.BindDeliveryRun(context.Background(), nil, id.NewAgentRunID()), ErrDeliveryBindingUnavailable)
}
