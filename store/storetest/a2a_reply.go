package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

func testA2AReservedReplyCommit(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, private, claimed, replies := prepareReservedReply(t, backend)
	invalid, err := replies.CommitReservedReply(ctx, claimed, string([]byte{0xff}), true)
	require.ErrorIs(t, err, a2a.ErrReplyCommitConflict)
	require.Zero(t, invalid)
	receipt, err := replies.CommitReservedReply(ctx, claimed, "checked", true)
	require.NoError(t, err)
	require.True(t, receipt.Fresh)
	require.Equal(t, claimed.RunID, receipt.RunID)
	retained, err := private.GetDelivery(ctx, claimed.ID)
	require.NoError(t, err)
	require.Equal(t, a2a.DeliveryDelivered, retained.State)
	queued, err := private.ListQueuedDeliveries(ctx, 10)
	require.NoError(t, err)
	require.Len(t, queued, 1)
	require.Equal(t, receipt.ReplyDeliveryID, queued[0].ID)
	message, err := private.GetMessage(ctx, receipt.ReplyMessageID)
	require.NoError(t, err)
	require.Equal(t, "checked", message.Content)
	require.Equal(t, []a2a.Address{{Agent: "planner"}}, message.Receivers)
	// The same completed write can be observed after a lost acknowledgment.
	again, err := replies.CommitReservedReply(ctx, claimed, "checked", true)
	require.NoError(t, err)
	require.False(t, again.Fresh)
	require.Equal(t, receipt.ReplyMessageID, again.ReplyMessageID)
	_, err = replies.CommitReservedReply(ctx, claimed, "different", true)
	require.ErrorIs(t, err, a2a.ErrReplyCommitConflict)
	observed, err := replies.GetReservedReply(ctx, claimed.ID)
	require.NoError(t, err)
	require.Equal(t, receipt.ReplyMessageID, observed.ReplyMessageID)
	_, err = replies.GetReservedReply(context.Background(), claimed.ID)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = replies.GetReservedReply(ctxWithScope("other"), claimed.ID)
	require.ErrorIs(t, err, a2a.ErrReplyCommitNotFound)
}

func prepareReservedReply(t *testing.T, backend store.Store) (context.Context, a2a.Store, *a2a.Delivery, a2a.ReservedReplyStore) {
	t.Helper()
	ctx := ctxWithScope(id.NewMessageID().String(), "project")
	team := id.NewOrchestrationID()
	private, err := a2a.NewRoutingStore(backend, team)
	require.NoError(t, err)
	replies, ok := private.(a2a.ReservedReplyStore)
	require.True(t, ok)
	bus, err := a2a.NewBus(a2a.BusConfig{Store: backend, Runner: routingRejectRunner{}, OrchestrationID: team, Synchronous: true})
	require.NoError(t, err)
	sent, err := bus.Send(ctx, a2a.SendParams{Sender: a2a.Address{Agent: "planner"}, Receivers: []a2a.Address{{Agent: "worker"}}, Performative: a2a.Request, Content: "evaluate"})
	require.NoError(t, err)
	claimed, err := private.ClaimDelivery(ctx, sent.Deliveries[0].DeliveryID)
	require.NoError(t, err)
	binder, ok := private.(a2a.DeliveryRunBinder)
	require.True(t, ok)
	child := id.NewAgentRunID()
	require.NoError(t, binder.BindDeliveryRun(ctx, claimed, child))
	claimed.RunID = child

	return ctx, private, claimed, replies
}

type messagingFaultDriver struct {
	assurance.Driver
	transaction assurance.MessagingDriver
	rollback    bool
	loseAck     bool
}

func (d messagingFaultDriver) TransactMessaging(ctx context.Context, delivery id.DeliveryID, conversation id.ConversationID, callback func(context.Context, assurance.MessagingTransaction) error) error {
	err := d.transaction.TransactMessaging(ctx, delivery, conversation, func(tc context.Context, tx assurance.MessagingTransaction) error {
		if callbackErr := callback(tc, tx); callbackErr != nil {
			return callbackErr
		}
		if d.rollback {
			return errors.New("injected rollback")
		}
		return nil
	})
	if err == nil && d.loseAck {
		return errors.New("injected lost acknowledgment")
	}
	return err
}

// A2AReplyTransactions checks native rollback and ambiguous commit recovery.
func A2AReplyTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	transactional, ok := driver.(assurance.MessagingDriver)
	require.True(t, ok)
	for _, rollback := range []bool{true, false} {
		name := "lost_ack"
		if rollback {
			name = "rollback"
		}
		t.Run(name, func(t *testing.T) {
			ctx, private, claimed, replies := prepareReservedReply(t, backend)
			faulty := assurance.New(messagingFaultDriver{Driver: driver, transaction: transactional, rollback: rollback, loseAck: !rollback})
			wrapper := &bindingFaultStore{Store: backend, replies: faulty}
			team, err := id.ParseOrchestrationID(claimed.OrchestrationID)
			require.NoError(t, err)
			bound, err := a2a.NewRoutingStore(wrapper, team)
			require.NoError(t, err)
			failing, ok := bound.(a2a.ReservedReplyStore)
			require.True(t, ok)
			receipt, err := failing.CommitReservedReply(ctx, claimed, "answer", true)
			require.Error(t, err)
			require.Zero(t, receipt)
			original, err := private.GetMessage(ctx, claimed.MessageID)
			require.NoError(t, err)
			conversation, err := private.GetConversation(ctx, original.ConversationID)
			require.NoError(t, err)
			retained, err := private.GetDelivery(ctx, claimed.ID)
			require.NoError(t, err)
			queue, err := private.ListQueuedDeliveries(ctx, 10)
			require.NoError(t, err)
			observed, observeErr := replies.GetReservedReply(ctx, claimed.ID)
			if rollback {
				require.Equal(t, 1, conversation.HopsUsed)
				require.Equal(t, a2a.DeliveryDelivering, retained.State)
				require.Empty(t, queue)
				require.ErrorIs(t, observeErr, a2a.ErrReplyCommitNotFound)
			} else {
				require.Equal(t, 2, conversation.HopsUsed)
				require.Equal(t, a2a.DeliveryDelivered, retained.State)
				require.Len(t, queue, 1)
				require.NoError(t, observeErr)
				require.False(t, observed.Fresh)
			}
			retried, err := replies.CommitReservedReply(ctx, claimed, "answer", true)
			require.NoError(t, err)
			require.Equal(t, rollback, retried.Fresh)
			if !rollback {
				require.Equal(t, observed.ReplyMessageID, retried.ReplyMessageID)
			}
		})
	}
}

func testA2AReplyConcurrency(t *testing.T, newStore func(*testing.T) store.Store) {
	ctx, private, claimed, replies := prepareReservedReply(t, newStore(t))
	var wg sync.WaitGroup
	results := make(chan a2a.ReservedReply, 4)
	failures := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			receipt, err := replies.CommitReservedReply(ctx, claimed, "answer", true)
			results <- receipt
			failures <- err
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		require.NoError(t, err)
	}
	fresh := 0
	var messageID id.MessageID
	for receipt := range results {
		if receipt.Fresh {
			fresh++
		}
		if messageID.IsNil() {
			messageID = receipt.ReplyMessageID
		}
		require.Equal(t, messageID, receipt.ReplyMessageID)
	}
	require.Equal(t, 1, fresh)
	original, err := private.GetMessage(ctx, claimed.MessageID)
	require.NoError(t, err)
	conversation, err := private.GetConversation(ctx, original.ConversationID)
	require.NoError(t, err)
	conversation.HopCeiling = 4
	require.NoError(t, private.UpdateConversation(ctx, conversation))
	errorsOut := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			msg := *original
			msg.ID = id.NewMessageID()
			_, recordErr := replies.RecordMessage(ctx, &msg)
			errorsOut <- recordErr
		})
	}
	wg.Wait()
	close(errorsOut)
	admitted := 0
	for recordErr := range errorsOut {
		if recordErr == nil {
			admitted++
		} else {
			require.ErrorIs(t, recordErr, a2a.ErrHopCeiling)
		}
	}
	require.Equal(t, 2, admitted)
	conversation, err = private.GetConversation(ctx, original.ConversationID)
	require.NoError(t, err)
	require.Equal(t, 4, conversation.HopsUsed)
	_, err = replies.RecordMessage(ctx, original)
	require.NoError(t, err, "identical retry does not consume another hop")
}

func testA2AReplyLostAcknowledgment(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx := ctxWithScope("reply-lost-ack")
	team, child := id.NewOrchestrationID(), id.NewAgentRunID()
	binder, ok := backend.(a2a.DeliveryRunBinder)
	require.True(t, ok)
	replies, ok := backend.(a2a.ReservedReplyStore)
	require.True(t, ok)
	wrapped := &bindingFaultStore{Store: backend, replies: replies, bind: binder.BindDeliveryRun}
	wrapped.commit = func(callCtx context.Context, delivery *a2a.Delivery, content string, success bool) (a2a.ReservedReply, error) {
		_, err := replies.CommitReservedReply(callCtx, delivery, content, success)
		if err != nil {
			return a2a.ReservedReply{}, err
		}
		return a2a.ReservedReply{}, errors.New("lost commit acknowledgment")
	}
	executed := 0
	runner := reservedMessageRunner{reserve: func(context.Context, id.OrchestrationID, string, string, *cortex.RunOpts) (*cortex.ReservedInvocation, error) {
		return &cortex.ReservedInvocation{RunID: child, Execute: func(context.Context) (*cortex.AgentResult, error) {
			executed++
			return &cortex.AgentResult{RunID: child, AgentName: "worker", Output: "checked"}, nil
		}}, nil
	}}
	resumer := &rosterResumer{}
	cfg := a2a.BusConfig{Store: wrapped, ReservedRunner: runner, Resumer: resumer, OrchestrationID: team, Synchronous: true}
	first, err := a2a.NewBus(cfg)
	require.NoError(t, err)
	_, err = first.Ask(ctx, a2a.AskParams{SendParams: a2a.SendParams{Sender: a2a.Address{Agent: "planner"}, Receivers: []a2a.Address{{Agent: "worker"}}, Performative: a2a.Request, Content: "evaluate"}, AskerRunID: id.NewAgentRunID(), ToolCallID: "ask-worker"})
	require.NoError(t, err)
	_, err = first.Drain(ctx)
	require.Error(t, err)
	require.Equal(t, 1, executed)
	require.Zero(t, resumer.calls)
	cfg.Store = backend
	recovered, err := a2a.NewBus(cfg)
	require.NoError(t, err)
	count, err := recovered.Drain(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Equal(t, 1, executed, "recovering publication must never rerun the recipient")
	require.Equal(t, 1, resumer.calls)
	inbox, err := recovered.Inbox(ctx, "planner", a2a.InboxFilter{UnreadOnly: true})
	require.NoError(t, err)
	require.Empty(t, inbox, "reply already consumed by resume must not appear unread")
	count, err = recovered.Drain(ctx)
	require.NoError(t, err)
	require.Zero(t, count)
	require.Equal(t, 1, resumer.calls)
}

func testA2AReplyAdmission(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, closed := range []bool{false, true} {
		ctx, private, claimed, replies := prepareReservedReply(t, newStore(t))
		original, err := private.GetMessage(ctx, claimed.MessageID)
		require.NoError(t, err)
		conv, err := private.GetConversation(ctx, original.ConversationID)
		require.NoError(t, err)
		expected := a2a.ErrHopCeiling
		conv.HopCeiling = conv.HopsUsed
		if closed {
			conv.Status = a2a.StatusClosed
			expected = a2a.ErrConversationClosed
		}
		require.NoError(t, private.UpdateConversation(ctx, conv))
		receipt, err := replies.CommitReservedReply(ctx, claimed, "answer", true)
		require.ErrorIs(t, err, expected)
		require.Zero(t, receipt)
		retained, err := private.GetDelivery(ctx, claimed.ID)
		require.NoError(t, err)
		require.Equal(t, a2a.DeliveryDelivering, retained.State)
		_, err = replies.GetReservedReply(ctx, claimed.ID)
		require.ErrorIs(t, err, a2a.ErrReplyCommitNotFound)
		queue, err := private.ListQueuedDeliveries(ctx, 10)
		require.NoError(t, err)
		require.Empty(t, queue)
	}
}
