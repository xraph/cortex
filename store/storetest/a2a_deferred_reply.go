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
)

type recipientObserver func(context.Context, a2a.RecipientRequest) (a2a.RecipientObservation, error)

type staleRecipientStore struct {
	*bindingFaultStore
	delivery *a2a.Delivery
}

func (s *staleRecipientStore) GetDelivery(ctx context.Context, key id.DeliveryID) (*a2a.Delivery, error) {
	if key == s.delivery.ID {
		copied := *s.delivery
		return &copied, nil
	}
	return s.Store.GetDelivery(ctx, key)
}

func (f recipientObserver) ObserveRecipient(ctx context.Context, req a2a.RecipientRequest) (a2a.RecipientObservation, error) {
	return f(ctx, req)
}

func testA2ADeferredReplyConcurrency(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, _, delivery, _ := prepareReservedReply(t, backend)
	team, err := id.ParseOrchestrationID(delivery.OrchestrationID)
	require.NoError(t, err)
	runner := reservedMessageRunner{reserve: func(context.Context, id.OrchestrationID, string, string, *cortex.RunOpts) (*cortex.ReservedInvocation, error) {
		t.Error("recovery tried to reserve")
		return nil, cortex.ErrRunIncomplete
	}}
	observer := recipientObserver(func(_ context.Context, req a2a.RecipientRequest) (a2a.RecipientObservation, error) {
		return a2a.RecipientObservation{RunID: req.RunID, AgentName: req.AgentName, State: a2a.RecipientSucceeded, Output: "one answer"}, nil
	})
	bus, err := a2a.NewBus(a2a.BusConfig{Store: backend, ReservedRunner: runner, RecipientObserver: observer, OrchestrationID: team, Synchronous: true})
	require.NoError(t, err)
	allow := func(context.Context, id.OrchestrationID, id.DeliveryID) error { return nil }
	_, err = bus.RecoverRecipient(ctx, delivery.ID, nil)
	require.ErrorIs(t, err, a2a.ErrRecipientObservationUnavailable)
	_, err = bus.RecoverRecipient(context.Background(), delivery.ID, allow)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = bus.RecoverRecipient(ctxWithScope("foreign"), delivery.ID, allow)
	require.Error(t, err)
	var wg sync.WaitGroup
	results := make(chan a2a.ReservedReply, 4)
	failures := make(chan error, 4)
	for range 4 {
		wg.Go(func() {
			result, recoverErr := bus.RecoverRecipient(ctx, delivery.ID, allow)
			results <- result
			failures <- recoverErr
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	for recoverErr := range failures {
		require.NoError(t, recoverErr)
	}
	fresh := 0
	var replyID id.MessageID
	for result := range results {
		if result.Fresh {
			fresh++
		}
		if replyID.IsNil() {
			replyID = result.ReplyMessageID
		}
		require.Equal(t, replyID, result.ReplyMessageID)
	}
	require.Equal(t, 1, fresh)
	// Another worker can commit after this worker reads the delivering row but
	// before it reads the receipt. The durable receipt wins over that stale read.
	binder, available := backend.(a2a.DeliveryRunBinder)
	require.True(t, available)
	replies, available := backend.(a2a.ReservedReplyStore)
	require.True(t, available)
	stale := &staleRecipientStore{bindingFaultStore: &bindingFaultStore{Store: backend, replies: replies, bind: binder.BindDeliveryRun}, delivery: delivery}
	bus, err = a2a.NewBus(a2a.BusConfig{Store: stale, ReservedRunner: runner, RecipientObserver: observer, OrchestrationID: team, Synchronous: true})
	require.NoError(t, err)
	prior, err := bus.RecoverRecipient(ctx, delivery.ID, allow)
	require.NoError(t, err)
	require.Equal(t, replyID, prior.ReplyMessageID)
	require.False(t, prior.Fresh)
}

func testA2ADeferredReply(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"success", "failure", "waiting", "observation_error", "wrong_run", "wrong_agent", "unknown_state", "denied", "revoked", "lost_ack"} {
		t.Run(mode, func(t *testing.T) {
			backend := newStore(t)
			ctx, private, delivery, replies := prepareReservedReply(t, backend)
			team, err := id.ParseOrchestrationID(delivery.OrchestrationID)
			require.NoError(t, err)
			original, err := private.GetMessage(ctx, delivery.MessageID)
			require.NoError(t, err)
			reserved, observed, checks := 0, 0, 0
			denied := errors.New("host permission revoked")
			reader := recipientObserver(func(_ context.Context, req a2a.RecipientRequest) (a2a.RecipientObservation, error) {
				observed++
				require.Equal(t, team, req.OrchestrationID)
				require.Equal(t, delivery.RunID, req.RunID)
				require.Equal(t, delivery.Receiver.Agent, req.AgentName)
				result := a2a.RecipientObservation{RunID: req.RunID, AgentName: req.AgentName, State: a2a.RecipientSucceeded, Output: "retained answer"}
				switch mode {
				case "failure":
					result.State, result.Output = a2a.RecipientFailed, "private failure details"
				case "waiting":
					result.State = a2a.RecipientWaiting
				case "observation_error":
					return result, errors.New("cannot read durable result")
				case "wrong_run":
					result.RunID = id.NewAgentRunID()
				case "wrong_agent":
					result.AgentName = "another worker"
				case "unknown_state":
					result.State = "unknown"
				}
				return result, nil
			})
			runner := reservedMessageRunner{reserve: func(context.Context, id.OrchestrationID, string, string, *cortex.RunOpts) (*cortex.ReservedInvocation, error) {
				reserved++
				return nil, cortex.ErrRunIncomplete
			}}
			binder, available := backend.(a2a.DeliveryRunBinder)
			require.True(t, available)
			wrapped := &bindingFaultStore{Store: backend, replies: replies, bind: binder.BindDeliveryRun}
			if mode == "lost_ack" {
				wrapped.commit = func(tc context.Context, d *a2a.Delivery, content string, success bool) (a2a.ReservedReply, error) {
					_, commitErr := replies.CommitReservedReply(tc, d, content, success)
					if commitErr != nil {
						return a2a.ReservedReply{}, commitErr
					}
					return a2a.ReservedReply{}, errors.New("lost acknowledgment")
				}
			}
			config := a2a.BusConfig{Store: wrapped, ReservedRunner: runner, RecipientObserver: reader, OrchestrationID: team, Synchronous: true}
			bus, err := a2a.NewBus(config)
			require.NoError(t, err)
			authorize := func(context.Context, id.OrchestrationID, id.DeliveryID) error {
				checks++
				if mode == "denied" || (mode == "revoked" && observed > 0) {
					return denied
				}
				return nil
			}
			result, recoverErr := bus.RecoverRecipient(ctx, delivery.ID, authorize)
			terminal := mode == "success" || mode == "failure" || mode == "lost_ack"
			if mode == "success" || mode == "failure" || mode == "waiting" {
				require.NoError(t, recoverErr)
			} else {
				require.Error(t, recoverErr)
				require.Zero(t, result)
			}
			if mode == "denied" {
				require.Zero(t, observed)
			}
			retained, err := private.GetDelivery(ctx, delivery.ID)
			require.NoError(t, err)
			require.Equal(t, delivery.RunID, retained.RunID)
			messages, err := private.ListMessages(ctx, &a2a.MessageListFilter{ConversationID: original.ConversationID})
			require.NoError(t, err)
			if !terminal {
				require.Equal(t, a2a.DeliveryDelivering, retained.State)
				require.Len(t, messages, 1)
				require.Zero(t, reserved)
				return
			}
			require.Equal(t, a2a.DeliveryDelivered, retained.State)
			require.Len(t, messages, 2)
			if mode == "failure" {
				require.NotContains(t, messages[1].Content, "private failure details")
				require.Equal(t, a2a.Failure, messages[1].Performative)
			} else {
				require.Equal(t, "retained answer", messages[1].Content)
			}
			config.Store = backend
			fresh, err := a2a.NewBus(config)
			require.NoError(t, err)
			before := observed
			again, err := fresh.RecoverRecipient(ctx, delivery.ID, authorize)
			require.NoError(t, err)
			require.Equal(t, before, observed, "a committed reply is an observation, not another recipient read")
			require.False(t, again.Fresh)
			require.Equal(t, delivery.RunID, again.RunID)
			_, err = fresh.Drain(ctx)
			require.NoError(t, err)
			inbox, err := fresh.Inbox(ctx, "planner", a2a.InboxFilter{UnreadOnly: true})
			require.NoError(t, err)
			require.Len(t, inbox, 1)
			require.Zero(t, reserved)
			require.Greater(t, checks, 1)
		})
	}
}
