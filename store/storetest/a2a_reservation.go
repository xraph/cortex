package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

func testA2ABoundDeliveryCannotBeReclaimed(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx := ctxWithScope("bound-delivery")
	conv := newA2AConversation("planner")
	require.NoError(t, s.CreateConversation(ctx, conv))
	msg := newA2AEnvelope(conv.ID, "planner")
	require.NoError(t, s.CreateMessage(ctx, msg))
	d := &a2a.Delivery{ID: id.NewDeliveryID(), MessageID: msg.ID, Receiver: a2a.Address{Agent: "worker"}, State: a2a.DeliveryQueued}
	require.NoError(t, s.CreateDelivery(ctx, d))
	claimed, err := s.ClaimDelivery(ctx, d.ID)
	require.NoError(t, err)
	claimed.RunID = id.NewAgentRunID()
	require.NoError(t, s.UpdateDelivery(ctx, claimed))
	updated, err := s.GetDelivery(ctx, d.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.ClaimedAt, "updating delivery must retain its claim")
	count, err := s.ReclaimStaleDeliveries(context.Background(), time.Now().Add(time.Hour), 100)
	require.NoError(t, err)
	require.Zero(t, count, "a timeout cannot authorize replay after binding an execution")
	retained, err := s.GetDelivery(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, claimed.RunID, retained.RunID)
	require.Equal(t, a2a.DeliveryDelivering, retained.State)
	// Older writers may have left an execution-bound row marked queued. It
	// must not make Drain repeatedly discover work it can never safely claim.
	legacy := &a2a.Delivery{ID: id.NewDeliveryID(), MessageID: msg.ID, State: a2a.DeliveryQueued, RunID: id.NewAgentRunID()}
	require.NoError(t, s.CreateDelivery(ctx, legacy))
	queue, err := s.ListQueuedDeliveries(context.Background(), 100)
	require.NoError(t, err)
	require.Empty(t, queue)
}

func testA2ADeliveryRunBinding(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	binder, supported := s.(a2a.DeliveryRunBinder)
	require.True(t, supported, "backend must atomically bind run admission")
	ctx := ctxWithScope("delivery-binding", "project")
	conv := newA2AConversation("planner")
	require.NoError(t, s.CreateConversation(ctx, conv))
	msg := newA2AEnvelope(conv.ID, "planner")
	require.NoError(t, s.CreateMessage(ctx, msg))
	d := &a2a.Delivery{ID: id.NewDeliveryID(), MessageID: msg.ID, State: a2a.DeliveryQueued}
	require.NoError(t, s.CreateDelivery(ctx, d))
	first, err := s.ClaimDelivery(ctx, d.ID)
	require.NoError(t, err)
	require.NotEmpty(t, first.ClaimToken)
	count, err := s.ReclaimStaleDeliveries(context.Background(), time.Now().Add(time.Hour), 100)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	second, err := s.ClaimDelivery(ctx, d.ID)
	require.NoError(t, err)
	require.NotEqual(t, first.ClaimToken, second.ClaimToken)
	key := id.NewAgentRunID()
	require.ErrorIs(t, binder.BindDeliveryRun(ctx, first, key), a2a.ErrDeliveryClaimLost)
	require.ErrorIs(t, s.UpdateDelivery(ctx, first), a2a.ErrDeliveryClaimLost)
	require.ErrorIs(t, binder.BindDeliveryRun(ctxWithScope("another"), second, key), a2a.ErrDeliveryNotFound)
	require.ErrorIs(t, binder.BindDeliveryRun(ctxWithScope("delivery-binding"), second, key), a2a.ErrDeliveryNotFound)
	require.ErrorIs(t, s.UpdateDelivery(ctxWithScope("delivery-binding"), second), a2a.ErrDeliveryNotFound)
	require.ErrorIs(t, binder.BindDeliveryRun(context.Background(), second, key), cortex.ErrNoScope)
	other, err := a2a.NewRoutingStore(s, id.NewOrchestrationID())
	require.NoError(t, err)
	otherBinder, ok := other.(a2a.DeliveryRunBinder)
	require.True(t, ok)
	require.ErrorIs(t, otherBinder.BindDeliveryRun(ctx, second, key), a2a.ErrDeliveryNotFound)
	require.ErrorIs(t, binder.BindDeliveryRun(ctx, second, id.NewAgentID()), a2a.ErrDeliveryClaimLost)
	const workers = 12
	results := make(chan error, workers)
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() { results <- binder.BindDeliveryRun(ctx, second, key) })
	}
	wg.Wait()
	close(results)
	winners := 0
	for bindErr := range results {
		if bindErr == nil {
			winners++
		} else {
			require.ErrorIs(t, bindErr, a2a.ErrDeliveryClaimLost)
		}
	}
	require.Equal(t, 1, winners)
	retained, err := s.GetDelivery(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, key, retained.RunID)
	count, err = s.ReclaimStaleDeliveries(context.Background(), time.Now().Add(time.Hour), 100)
	require.NoError(t, err)
	require.Zero(t, count)
	retained.RunID = id.Nil
	require.ErrorIs(t, s.UpdateDelivery(ctx, retained), a2a.ErrDeliveryClaimLost)
}

type reservedMessageRunner struct {
	reserve func(context.Context, id.OrchestrationID, string, string, *cortex.RunOpts) (*cortex.ReservedInvocation, error)
}

func (r reservedMessageRunner) ReserveAgent(ctx context.Context, team id.OrchestrationID, name, input string, opts *cortex.RunOpts) (*cortex.ReservedInvocation, error) {
	return r.reserve(ctx, team, name, input, opts)
}

func testA2AReservedDispatch(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx := ctxWithScope("reserved-dispatch")
	team, child := id.NewOrchestrationID(), id.NewAgentRunID()
	private, err := a2a.NewRoutingStore(backend, team)
	require.NoError(t, err)
	var deliveryID id.DeliveryID
	reserved, executed := 0, 0
	runner := reservedMessageRunner{reserve: func(callCtx context.Context, gotTeam id.OrchestrationID, name, input string, opts *cortex.RunOpts) (*cortex.ReservedInvocation, error) {
		reserved++
		require.Equal(t, team, gotTeam)
		require.Equal(t, "worker", name)
		require.Contains(t, input, "evaluate")
		require.Nil(t, opts)
		before, readErr := private.GetDelivery(callCtx, deliveryID)
		require.NoError(t, readErr)
		require.True(t, before.RunID.IsNil())
		return &cortex.ReservedInvocation{RunID: child, Execute: func(execCtx context.Context) (*cortex.AgentResult, error) {
			executed++
			stored, getErr := private.GetDelivery(execCtx, deliveryID)
			require.NoError(t, getErr)
			require.Equal(t, child, stored.RunID, "identity must be durable before dispatch")
			require.NotEmpty(t, stored.ClaimToken)
			count, reclaimErr := private.ReclaimStaleDeliveries(context.Background(), time.Now().Add(time.Hour), 100)
			require.NoError(t, reclaimErr)
			require.Zero(t, count)
			return &cortex.AgentResult{RunID: child, AgentName: name, Output: "checked"}, nil
		}}, nil
	}}
	bus, err := a2a.NewBus(a2a.BusConfig{Store: backend, ReservedRunner: runner, OrchestrationID: team, Roster: &a2a.Roster{Scope: cortex.ScopeFromContext(ctx), Participants: []a2a.RosterParticipant{{Name: "planner", AgentID: id.NewAgentID()}, {Name: "worker", AgentID: id.NewAgentID()}}}, Synchronous: true})
	require.NoError(t, err)
	sent, err := bus.Send(ctx, a2a.SendParams{Sender: a2a.Address{Agent: "planner"}, Receivers: []a2a.Address{{Agent: "worker"}}, Performative: a2a.Request, Content: "evaluate"})
	require.NoError(t, err)
	queued, err := private.ListQueuedDeliveries(ctx, 10)
	require.NoError(t, err)
	require.Len(t, queued, 1)
	deliveryID = queued[0].ID
	_, err = bus.Drain(context.Background())
	require.NoError(t, err)
	require.Equal(t, 1, reserved)
	require.Equal(t, 1, executed)
	retained, err := private.GetDelivery(ctx, deliveryID)
	require.NoError(t, err)
	require.Equal(t, child, retained.RunID)
	replies, err := private.ListMessages(ctx, &a2a.MessageListFilter{ConversationID: sent.ConversationID})
	require.NoError(t, err)
	require.Len(t, replies, 2)
	require.Equal(t, "checked", replies[1].Content)
}

type bindingFaultStore struct {
	store.Store
	bind func(context.Context, *a2a.Delivery, id.AgentRunID) error
}

func (*bindingFaultStore) SupportsOrchestrationRouting() bool { return true }

func (s *bindingFaultStore) BindDeliveryRun(ctx context.Context, d *a2a.Delivery, key id.AgentRunID) error {
	return s.bind(ctx, d, key)
}

func testA2AReservedDispatchFailures(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"bind_failure", "lost_ack", "reserve_failure", "cancelled", "stale_claim", "wrong_result", "mutated_reservation", "result_error", "nil_result", "waiting", "mixed_waiting"} {
		t.Run(mode, func(t *testing.T) {
			backend := newStore(t)
			ctx := ctxWithScope("reserved-failure")
			team, child, unexpected := id.NewOrchestrationID(), id.NewAgentRunID(), id.NewAgentRunID()
			private, err := a2a.NewRoutingStore(backend, team)
			require.NoError(t, err)
			binder, ok := backend.(a2a.DeliveryRunBinder)
			require.True(t, ok)
			failure := errors.New("private diagnostic")
			wrapped := &bindingFaultStore{Store: backend, bind: func(bindCtx context.Context, d *a2a.Delivery, key id.AgentRunID) error {
				if mode == "bind_failure" {
					return failure
				}
				if bindErr := binder.BindDeliveryRun(bindCtx, d, key); bindErr != nil {
					return bindErr
				}
				if mode == "lost_ack" {
					return failure
				}
				return nil
			}}
			drainCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			var deliveryID id.DeliveryID
			executed := 0
			runner := reservedMessageRunner{reserve: func(callCtx context.Context, _ id.OrchestrationID, name, _ string, _ *cortex.RunOpts) (*cortex.ReservedInvocation, error) {
				if mode == "cancelled" {
					cancel()
				}
				if mode == "stale_claim" {
					count, reclaimErr := private.ReclaimStaleDeliveries(callCtx, time.Now().Add(time.Hour), 100)
					require.NoError(t, reclaimErr)
					require.Equal(t, 1, count)
					_, reclaimErr = private.ClaimDelivery(callCtx, deliveryID)
					require.NoError(t, reclaimErr)
				}
				var inv *cortex.ReservedInvocation
				inv = &cortex.ReservedInvocation{RunID: child, Execute: func(context.Context) (*cortex.AgentResult, error) {
					executed++
					if mode == "nil_result" {
						return nil, nil
					}
					result := &cortex.AgentResult{RunID: child, AgentName: name, Output: "private output"}
					if mode == "wrong_result" || mode == "mutated_reservation" {
						result.RunID = unexpected
					}
					if mode == "mutated_reservation" {
						inv.RunID = unexpected
					}
					if mode == "result_error" {
						result.Err = failure
					}
					if mode == "waiting" || mode == "mixed_waiting" {
						result.Output = ""
						result.Err = cortex.ErrOrchestrationWaiting
						if mode == "mixed_waiting" {
							return result, failure
						}
						return result, cortex.ErrOrchestrationWaiting
					}
					return result, nil
				}}
				if mode == "reserve_failure" {
					return inv, failure
				}
				return inv, nil
			}}
			resumer := &rosterResumer{}
			bus, err := a2a.NewBus(a2a.BusConfig{Store: wrapped, ReservedRunner: runner, Resumer: resumer, OrchestrationID: team, Synchronous: true})
			require.NoError(t, err)
			sent, err := bus.Ask(ctx, a2a.AskParams{SendParams: a2a.SendParams{Sender: a2a.Address{Agent: "planner"}, Receivers: []a2a.Address{{Agent: "worker"}}, Performative: a2a.Request, Content: "evaluate"}, AskerRunID: id.NewAgentRunID(), ToolCallID: "ask-worker"})
			require.NoError(t, err)
			queued, err := private.ListQueuedDeliveries(ctx, 10)
			require.NoError(t, err)
			require.Len(t, queued, 1)
			deliveryID = queued[0].ID
			_, drainErr := bus.Drain(drainCtx)
			wantExecution := mode == "wrong_result" || mode == "mutated_reservation" || mode == "result_error" || mode == "nil_result" || mode == "waiting" || mode == "mixed_waiting"
			if wantExecution {
				require.Equal(t, 1, executed)
			} else {
				require.Zero(t, executed)
			}
			if mode == "result_error" || mode == "nil_result" || mode == "waiting" || mode == "mixed_waiting" {
				require.NoError(t, drainErr)
			} else {
				require.Error(t, drainErr)
			}
			if !wantExecution {
				var identity interface{ RunIdentity() id.AgentRunID }
				require.ErrorAs(t, drainErr, &identity)
				require.Equal(t, child, identity.RunIdentity())
				require.NotContains(t, drainErr.Error(), "private diagnostic")
			}
			retained, err := private.GetDelivery(ctx, deliveryID)
			require.NoError(t, err)
			if mode == "bind_failure" || mode == "stale_claim" {
				require.True(t, retained.RunID.IsNil())
			} else {
				require.Equal(t, child, retained.RunID)
			}
			if mode == "wrong_result" || mode == "mutated_reservation" {
				require.Contains(t, retained.Error, unexpected.String())
			}
			messages, err := private.ListMessages(ctx, &a2a.MessageListFilter{ConversationID: sent.ConversationID})
			require.NoError(t, err)
			switch mode {
			case "waiting":
				require.Len(t, messages, 1, "suspension must not publish a terminal reply")
				require.Zero(t, resumer.calls, "the sender must remain suspended for a real reply")
				pending, pendingErr := private.ListPendingAsksByConversation(ctx, sent.ConversationID)
				require.NoError(t, pendingErr)
				require.Len(t, pending, 1)
				require.Equal(t, a2a.DeliveryDelivering, retained.State)
				require.Nil(t, retained.DeliveredAt)
				require.NotEmpty(t, retained.ClaimToken)
				_, drainErr = bus.Drain(ctx)
				require.NoError(t, drainErr)
				require.Equal(t, 1, executed, "a retained child cannot be executed again")
			case "mixed_waiting":
				require.Len(t, messages, 2, "a mixed failure must not become a suspension")
				require.Equal(t, a2a.Failure, messages[1].Performative)
				require.Equal(t, 1, resumer.calls)
			}
			for _, message := range messages {
				require.NotContains(t, message.Content, "private")
			}
			if mode != "bind_failure" && mode != "stale_claim" {
				count, reclaimErr := private.ReclaimStaleDeliveries(ctx, time.Now().Add(time.Hour), 100)
				require.NoError(t, reclaimErr)
				require.Zero(t, count)
			}
		})
	}
}
