package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

// A2ARoutingUpgrade checks legacy records after removing their routing fields.
func A2ARoutingUpgrade(t *testing.T, s store.Store, legacy func()) {
	t.Helper()
	ctx := ctxWithScope("routing-upgrade")
	conv := newA2AConversation("sender")
	require.NoError(t, s.CreateConversation(ctx, conv))
	msg := newA2AEnvelope(conv.ID, "sender")
	require.NoError(t, s.CreateMessage(ctx, msg))
	delivery := &a2a.Delivery{ID: id.NewDeliveryID(), MessageID: msg.ID, Receiver: a2a.Address{Agent: "worker"}, State: a2a.DeliveryQueued}
	require.NoError(t, s.CreateDelivery(ctx, delivery))
	deadline := time.Now().Add(-time.Minute)
	ask := &a2a.PendingAsk{ReplyWith: id.NewMessageID().String(), ConversationID: conv.ID, MessageID: msg.ID, AskerRunID: id.NewAgentRunID(), Deadline: &deadline}
	require.NoError(t, s.CreatePendingAsk(ctx, ask))
	legacy()
	require.NoError(t, s.Migrate(context.Background()))
	require.NoError(t, s.Migrate(context.Background()))
	message, err := s.GetMessage(ctx, msg.ID)
	require.NoError(t, err)
	require.Empty(t, message.OrchestrationID)
	conversation, err := s.GetConversation(ctx, conv.ID)
	require.NoError(t, err)
	require.Empty(t, conversation.OrchestrationID)
	queued, err := s.ListQueuedDeliveries(context.Background(), 100)
	require.NoError(t, err)
	require.Len(t, queued, 1)
	require.Equal(t, delivery.ID, queued[0].ID)
	private, err := a2a.NewRoutingStore(s, id.NewOrchestrationID())
	require.NoError(t, err)
	_, err = private.GetMessage(ctx, msg.ID)
	require.ErrorIs(t, err, a2a.ErrMessageNotFound)
	rows, err := private.ListQueuedDeliveries(context.Background(), 100)
	require.NoError(t, err)
	require.Empty(t, rows)
	_, err = private.ClaimPendingAsk(ctx, ask.ReplyWith)
	require.ErrorIs(t, err, a2a.ErrAskNotFound)
	claimed, err := s.ClaimPendingAsk(ctx, ask.ReplyWith)
	require.NoError(t, err)
	require.Empty(t, claimed.OrchestrationID)
	claimedDelivery, err := s.ClaimDelivery(ctx, delivery.ID)
	require.NoError(t, err)
	require.NotEmpty(t, claimedDelivery.ClaimToken)
	binder, ok := s.(a2a.DeliveryRunBinder)
	require.True(t, ok)
	require.NoError(t, binder.BindDeliveryRun(ctx, claimedDelivery, id.NewAgentRunID()))
}

// A2ASQLRoutingUpgrade recreates the schema before routing partitions, then
// replays an unrecorded migration after its columns and indexes already exist.
func A2ASQLRoutingUpgrade(t *testing.T, s store.Store, exec func(string) error) {
	t.Helper()
	A2ARoutingUpgrade(t, s, func() {
		require.NoError(t, exec("ALTER TABLE cortex_a2a_deliveries DROP COLUMN claim_token"))
		require.NoError(t, exec("DELETE FROM grove_migrations WHERE version = '20261025000002' AND \"group\" = 'cortex'"))
		for _, name := range []string{"idx_a2a_routing_queue", "idx_a2a_routing_claim", "idx_a2a_routing_asks"} {
			require.NoError(t, exec("DROP INDEX "+name))
		}
		for _, suffix := range []string{"messages", "conversations", "deliveries", "pending_asks"} {
			table := "cortex_a2a_" + suffix
			require.NoError(t, exec("DROP INDEX idx_"+table+"_routing"))
			require.NoError(t, exec("ALTER TABLE "+table+" DROP COLUMN orchestration_id"))
		}
		require.NoError(t, exec("DELETE FROM grove_migrations WHERE version = '20261025000001' AND \"group\" = 'cortex'"))
	})
	require.NoError(t, exec("DELETE FROM grove_migrations WHERE version = '20261025000001' AND \"group\" = 'cortex'"))
	require.NoError(t, exec("DELETE FROM grove_migrations WHERE version = '20261025000002' AND \"group\" = 'cortex'"))
	require.NoError(t, s.Migrate(context.Background()))
}
