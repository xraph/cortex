package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

// Dropping the routing predicate would expose or consume another team's records.
func testA2ARoutingIsolation(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx := ctxWithScope("routing")
	firstID, secondID := id.NewOrchestrationID(), id.NewOrchestrationID()
	first, err := a2a.NewRoutingStore(backend, firstID)
	require.NoError(t, err)
	second, err := a2a.NewRoutingStore(backend, secondID)
	require.NoError(t, err)
	global, err := a2a.NewRoutingStore(backend, id.Nil)
	require.NoError(t, err)
	for ownIndex, own := range []a2a.Store{first, second, global} {
		expectedRoute := []string{firstID.String(), secondID.String(), ""}[ownIndex]
		conv := newA2AConversation("sender")
		conv.PeerNode, conv.PeerContext = "peer", "shared-thread"
		require.NoError(t, own.CreateConversation(ctx, conv))
		require.Equal(t, expectedRoute, conv.OrchestrationID)
		msg := newA2AEnvelope(conv.ID, "sender")
		require.NoError(t, own.CreateMessage(ctx, msg))
		storedMessage, getErr := own.GetMessage(ctx, msg.ID)
		require.NoError(t, getErr)
		require.Equal(t, expectedRoute, storedMessage.OrchestrationID)
		delivery := &a2a.Delivery{ID: id.NewDeliveryID(), MessageID: msg.ID, Receiver: a2a.Address{Agent: "worker"}, State: a2a.DeliveryQueued}
		require.NoError(t, own.CreateDelivery(ctx, delivery))
		deadline := time.Now().Add(-time.Minute)
		ask := &a2a.PendingAsk{ReplyWith: id.NewMessageID().String(), ConversationID: conv.ID, MessageID: msg.ID, AskerRunID: id.NewAgentRunID(), Expected: a2a.Address{Agent: "worker"}, Deadline: &deadline}
		require.NoError(t, own.CreatePendingAsk(ctx, ask))
		for otherIndex, other := range []a2a.Store{first, second, global} {
			if otherIndex == ownIndex {
				continue
			}
			_, err = other.GetMessage(ctx, msg.ID)
			require.ErrorIs(t, err, a2a.ErrMessageNotFound)
			_, err = other.GetConversation(ctx, conv.ID)
			require.ErrorIs(t, err, a2a.ErrConversationNotFound)
			require.ErrorIs(t, other.UpdateConversation(ctx, conv), a2a.ErrConversationNotFound)
			_, err = other.GetDelivery(ctx, delivery.ID)
			require.ErrorIs(t, err, a2a.ErrDeliveryNotFound)
			require.ErrorIs(t, other.UpdateDelivery(ctx, delivery), a2a.ErrDeliveryNotFound)
			require.ErrorIs(t, other.MarkDeliveryRead(ctx, delivery.ID), a2a.ErrDeliveryNotFound)
			_, err = other.ClaimDelivery(ctx, delivery.ID)
			require.ErrorIs(t, err, a2a.ErrDeliveryNotFound)
			_, err = other.ClaimPendingAsk(ctx, ask.ReplyWith)
			require.ErrorIs(t, err, a2a.ErrAskNotFound)
			messages, listErr := other.ListMessages(ctx, &a2a.MessageListFilter{ConversationID: conv.ID})
			require.NoError(t, listErr)
			require.Empty(t, messages)
			asks, listErr := other.ListPendingAsksByConversation(ctx, conv.ID)
			require.NoError(t, listErr)
			require.Empty(t, asks)
		}
		queued, listErr := own.ListQueuedDeliveries(context.Background(), 100)
		require.NoError(t, listErr)
		require.Len(t, queued, 1)
		require.Equal(t, expectedRoute, queued[0].OrchestrationID)
		require.Equal(t, delivery.ID, queued[0].ID)
		conversations, listErr := own.ListConversations(ctx, nil)
		require.NoError(t, listErr)
		require.Len(t, conversations, 1)
		peer, listErr := own.GetConversationByPeerContext(ctx, "peer", "shared-thread")
		require.NoError(t, listErr)
		require.Equal(t, conv.ID, peer.ID)
		expired, listErr := own.ListExpiredAsks(ctx, time.Now(), 100)
		require.NoError(t, listErr)
		require.Len(t, expired, 1)
		require.Equal(t, expectedRoute, expired[0].OrchestrationID)
		claimed, claimErr := own.ClaimDelivery(ctx, delivery.ID)
		require.NoError(t, claimErr)
		for otherIndex, other := range []a2a.Store{first, second, global} {
			if otherIndex == ownIndex {
				continue
			}
			count, reclaimErr := other.ReclaimStaleDeliveries(context.Background(), time.Now().Add(time.Hour), 100)
			require.NoError(t, reclaimErr)
			require.Zero(t, count)
		}
		claimed.State = a2a.DeliveryDelivered
		require.NoError(t, own.UpdateDelivery(ctx, claimed))
		inbox, listErr := own.ListInbox(ctx, "worker", a2a.InboxFilter{})
		require.NoError(t, listErr)
		require.Len(t, inbox, 1)
		require.Equal(t, delivery.ID, inbox[0].ID)
		require.Equal(t, expectedRoute, inbox[0].OrchestrationID)
		conv.OrchestrationID = "tampered"
		require.NoError(t, own.UpdateConversation(ctx, conv))
		unchanged, getErr := own.GetConversation(ctx, conv.ID)
		require.NoError(t, getErr)
		require.Equal(t, expectedRoute, unchanged.OrchestrationID)
	}
}

type routingRejectRunner struct{}

func (routingRejectRunner) RunAgent(context.Context, string, string, *cortex.RunOpts) (*cortex.AgentResult, error) {
	return nil, a2a.ErrUnroutable
}

// A bus must not inherit a different partition from its caller or backing wrapper.
func testA2ARoutingBus(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx := ctxWithScope("routing-bus")
	key := id.NewOrchestrationID()
	own, err := a2a.NewRoutingStore(backend, key)
	require.NoError(t, err)
	private, err := a2a.NewBus(a2a.BusConfig{Store: backend, Runner: routingRejectRunner{}, Synchronous: true, OrchestrationID: key})
	require.NoError(t, err)
	global, err := a2a.NewBus(a2a.BusConfig{Store: own, Runner: routingRejectRunner{}, Synchronous: true})
	require.NoError(t, err)
	sent, err := private.Send(ctx, a2a.SendParams{Sender: a2a.Address{Agent: "sender"}, Receivers: []a2a.Address{{Agent: "worker"}}, Performative: a2a.Inform, Content: "private"})
	require.NoError(t, err)
	_, err = backend.GetMessage(ctx, sent.MessageID)
	require.ErrorIs(t, err, a2a.ErrMessageNotFound)
	count, err := global.Drain(context.Background())
	require.NoError(t, err)
	require.Zero(t, count)
	// Reopen through a different bus to prove the partition survives process state.
	reopened, err := a2a.NewBus(a2a.BusConfig{Store: backend, Runner: routingRejectRunner{}, Synchronous: true, OrchestrationID: key})
	require.NoError(t, err)
	count, err = reopened.Drain(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, count)
	inbox, err := own.ListInbox(ctx, "worker", a2a.InboxFilter{})
	require.NoError(t, err)
	require.Len(t, inbox, 1)
	require.Equal(t, sent.MessageID, inbox[0].MessageID)
}
