package engine

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
)

type handoffPort struct{}

func (*handoffPort) ValidateHandoff(context.Context, *a2a.Envelope) error { return nil }
func (*handoffPort) ReceiveHandoff(context.Context, *a2a.Envelope) error  { return nil }
func TestHandoffOptionRequiresExplicitMessagingAndNonNilPort(t *testing.T) {
	for _, handler := range []a2a.HandoffHandler{nil, (*handoffPort)(nil), &handoffPort{}} {
		_, err := New(WithA2AHandoffHandler(handler))
		require.ErrorIs(t, err, cortex.ErrHandoffUnavailable)
	}
}
