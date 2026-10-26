package storetest

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

func recipientRecoveryBus(ctx context.Context, t *testing.T, backend store.Store, first *a2a.Delivery, observer a2a.RecipientObserver, config *a2a.RecipientRecoveryConfig, synchronous bool, clocks ...a2a.Clock) *a2a.Bus {
	t.Helper()
	team, err := id.ParseOrchestrationID(first.OrchestrationID)
	require.NoError(t, err)
	runner := reservedMessageRunner{reserve: func(context.Context, id.OrchestrationID, string, string, *cortex.RunOpts) (*cortex.ReservedInvocation, error) {
		t.Error("recovery reserved another child")
		return nil, cortex.ErrRunIncomplete
	}}
	var clock a2a.Clock
	if len(clocks) > 0 {
		clock = clocks[0]
	}
	bus, err := a2a.NewBus(a2a.BusConfig{Clock: clock, Store: backend, ReservedRunner: runner, RecipientObserver: observer, RecipientRecovery: config, OrchestrationID: team,
		Roster:      &a2a.Roster{Scope: cortex.ScopeFromContext(ctx), Participants: []a2a.RosterParticipant{{Name: "planner", AgentID: id.NewAgentID()}, {Name: "worker", AgentID: id.NewAgentID()}}},
		Synchronous: synchronous, Options: a2a.Options{SweepInterval: 10 * time.Millisecond, Workers: 1}})
	require.NoError(t, err)
	return bus
}

func allowRecipientRecovery() *a2a.RecipientRecoveryConfig {
	return &a2a.RecipientRecoveryConfig{
		BatchSize: 1, AttemptTimeout: time.Second,
		AuthorizeDiscovery: func(context.Context, id.OrchestrationID) error { return nil },
		AuthorizeDelivery:  func(context.Context, id.OrchestrationID, id.DeliveryID) error { return nil },
	}
}

func addBoundRecipient(ctx context.Context, t *testing.T, private a2a.Store, first *a2a.Delivery) *a2a.Delivery {
	t.Helper()
	delivery := &a2a.Delivery{Entity: cortex.NewEntity(), ID: id.NewDeliveryID(), Scope: cortex.ScopeFromContext(ctx), MessageID: first.MessageID, Receiver: first.Receiver, State: a2a.DeliveryQueued}
	require.NoError(t, private.CreateDelivery(ctx, delivery))
	claimed, err := private.ClaimDelivery(ctx, delivery.ID)
	require.NoError(t, err)
	binder, ok := private.(a2a.DeliveryRunBinder)
	require.True(t, ok)
	child := id.NewAgentRunID()
	require.NoError(t, binder.BindDeliveryRun(ctx, claimed, child))
	claimed.RunID = child
	return claimed
}

func testA2ARecipientDiscovery(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, private, first, _ := prepareReservedReply(t, backend)
	second := addBoundRecipient(ctx, t, private, first)
	// The same partition in a descendant scope must remain invisible.
	descendant := cortex.ScopeFromContext(ctx)
	descendant.Levels = append(descendant.Levels, cortex.Level{Key: "resource", Value: "other"})
	addBoundRecipient(cortex.WithScope(ctx, descendant), t, private, first)
	prepareReservedReply(t, backend) // Another scope and team.
	other, err := a2a.NewRoutingStore(backend, id.NewOrchestrationID())
	require.NoError(t, err)
	addBoundRecipient(ctx, t, other, first) // Same scope, different team.
	for _, claim := range []bool{false, true} {
		pending := &a2a.Delivery{Entity: cortex.NewEntity(), ID: id.NewDeliveryID(), Scope: cortex.ScopeFromContext(ctx), MessageID: first.MessageID, Receiver: first.Receiver, State: a2a.DeliveryQueued}
		require.NoError(t, private.CreateDelivery(ctx, pending))
		if claim {
			_, claimErr := private.ClaimDelivery(ctx, pending.ID)
			require.NoError(t, claimErr)
		}
	}
	reader, ok := private.(a2a.RecipientDiscoveryStore)
	require.True(t, ok)
	rows, err := reader.ListDeferredRecipients(ctx, id.Nil, 1)
	require.NoError(t, err)
	require.Equal(t, []id.DeliveryID{first.ID}, rows)
	rows, err = reader.ListDeferredRecipients(ctx, first.ID, 1)
	require.NoError(t, err)
	require.Equal(t, []id.DeliveryID{second.ID}, rows)
	rows, err = reader.ListDeferredRecipients(ctx, second.ID, 1)
	require.NoError(t, err)
	require.Empty(t, rows)
	replies, ok := private.(a2a.ReservedReplyStore)
	require.True(t, ok)
	_, err = replies.CommitReservedReply(ctx, first, "completed", true)
	require.NoError(t, err)
	rows, err = reader.ListDeferredRecipients(ctx, id.Nil, 10)
	require.NoError(t, err)
	require.Equal(t, []id.DeliveryID{second.ID}, rows)
	for _, limit := range []int{-1, 0, 257} {
		_, err = reader.ListDeferredRecipients(ctx, id.Nil, limit)
		require.Error(t, err)
	}
	_, err = reader.ListDeferredRecipients(context.Background(), id.Nil, 1)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = reader.ListDeferredRecipients(ctx, id.NewAgentRunID(), 1)
	require.Error(t, err)
	global, ok := backend.(a2a.RecipientDiscoveryStore)
	require.True(t, ok)
	rows, err = global.ListDeferredRecipients(ctx, id.Nil, 10)
	require.NoError(t, err)
	require.Empty(t, rows)
}

func testA2AAutomaticRecipientRecovery(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, private, first, replies := prepareReservedReply(t, backend)
	second := addBoundRecipient(ctx, t, private, first)
	var observations atomic.Int64
	observer := recipientObserver(func(_ context.Context, req a2a.RecipientRequest) (a2a.RecipientObservation, error) {
		observations.Add(1)
		state := a2a.RecipientSucceeded
		if req.RunID == first.RunID {
			state = a2a.RecipientWaiting
		}
		return a2a.RecipientObservation{RunID: req.RunID, AgentName: req.AgentName, State: state, Output: "retained answer"}, nil
	})
	bus := recipientRecoveryBus(ctx, t, backend, first, observer, allowRecipientRecovery(), false)
	require.NoError(t, bus.Start(context.Background()))
	t.Cleanup(bus.Stop)
	require.Eventually(t, func() bool {
		receipt, err := replies.GetReservedReply(ctx, second.ID)
		if err != nil {
			return false
		}
		delivered, err := private.GetDelivery(ctx, receipt.ReplyDeliveryID)
		return err == nil && delivered.State == a2a.DeliveryDelivered
	}, 5*time.Second, 10*time.Millisecond)
	bus.Stop()
	require.Positive(t, observations.Load())
	_, err := replies.GetReservedReply(ctx, first.ID)
	require.ErrorIs(t, err, a2a.ErrReplyCommitNotFound)
	// A fresh bus drains the retained reply without reserving or running a child.
	fresh := recipientRecoveryBus(ctx, t, backend, first, observer, allowRecipientRecovery(), true)
	_, err = fresh.Drain(ctx)
	require.NoError(t, err)
	receipt, err := replies.GetReservedReply(ctx, second.ID)
	require.NoError(t, err)
	delivered, err := private.GetDelivery(ctx, receipt.ReplyDeliveryID)
	require.NoError(t, err)
	require.Equal(t, a2a.DeliveryDelivered, delivered.State)
}

func testA2ARecipientRecoveryAuthority(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, _, first, _ := prepareReservedReply(t, backend)
	observed := 0
	observer := recipientObserver(func(_ context.Context, req a2a.RecipientRequest) (a2a.RecipientObservation, error) {
		observed++
		return a2a.RecipientObservation{RunID: req.RunID, AgentName: req.AgentName, State: a2a.RecipientWaiting}, nil
	})
	denied := errors.New("recovery denied")
	config := allowRecipientRecovery()
	checks := 0
	config.AuthorizeDiscovery = func(context.Context, id.OrchestrationID) error {
		checks++
		if checks > 1 {
			return denied
		}
		return nil
	}
	bus := recipientRecoveryBus(ctx, t, backend, first, observer, config, true)
	page, err := bus.RecoverRecipients(ctx, id.Nil)
	require.ErrorIs(t, err, denied)
	require.Zero(t, page)
	require.Zero(t, observed)
	config = allowRecipientRecovery()
	config.AuthorizeDelivery = func(context.Context, id.OrchestrationID, id.DeliveryID) error { return denied }
	bus = recipientRecoveryBus(ctx, t, backend, first, observer, config, true)
	page, err = bus.RecoverRecipients(ctx, id.Nil)
	require.ErrorIs(t, err, denied)
	require.Equal(t, first.ID, page.Next)
	require.Equal(t, 1, page.Failed)
	require.Zero(t, observed)
	_, err = bus.RecoverRecipients(ctxWithScope("foreign"), id.Nil)
	require.ErrorIs(t, err, a2a.ErrRosterDenied)
}

func testA2ARecipientRecoveryStop(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, _, first, _ := prepareReservedReply(t, backend)
	entered, cancelled, release, stopped := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	observer := recipientObserver(func(ctx context.Context, _ a2a.RecipientRequest) (a2a.RecipientObservation, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		<-release
		return a2a.RecipientObservation{}, ctx.Err()
	})
	bus := recipientRecoveryBus(ctx, t, backend, first, observer, allowRecipientRecovery(), false)
	require.NoError(t, bus.Start(context.Background()))
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("observer never started")
	}
	go func() { bus.Stop(); close(stopped) }()
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("observer did not receive cancellation")
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned while observation was active")
	default:
	}
	// Releasing via a value keeps the deferred close safe on early failures.
	release <- struct{}{}
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not join recovery")
	}
}

func testA2ARecipientRecoveryReclaimsReply(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, private, first, replies := prepareReservedReply(t, backend)
	observer := recipientObserver(func(_ context.Context, req a2a.RecipientRequest) (a2a.RecipientObservation, error) {
		return a2a.RecipientObservation{RunID: req.RunID, AgentName: req.AgentName, State: a2a.RecipientSucceeded, Output: "saved answer"}, nil
	})
	bus := recipientRecoveryBus(ctx, t, backend, first, observer, allowRecipientRecovery(), true)
	page, err := bus.RecoverRecipients(ctx, id.Nil)
	require.NoError(t, err)
	require.Equal(t, 1, page.Completed)
	receipt, err := replies.GetReservedReply(ctx, first.ID)
	require.NoError(t, err)
	_, err = private.ClaimDelivery(ctx, receipt.ReplyDeliveryID)
	require.NoError(t, err)
	// Simulate a dead dispatcher that claimed the retained reply and never carried it.
	fresh := recipientRecoveryBus(ctx, t, backend, first, observer, allowRecipientRecovery(), false, rosterClock{at: time.Now().Add(16 * time.Minute)})
	require.NoError(t, fresh.Start(context.Background()))
	t.Cleanup(fresh.Stop)
	require.Eventually(t, func() bool {
		delivery, readErr := private.GetDelivery(ctx, receipt.ReplyDeliveryID)
		return readErr == nil && delivery.State == a2a.DeliveryDelivered
	}, 2*time.Second, 10*time.Millisecond)
	fresh.Stop()
}

func testA2APrivateDispatchReadiness(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, private, first, replies := prepareReservedReply(t, backend)
	allowed := true
	denied := errors.New("discovery revoked")
	config := allowRecipientRecovery()
	config.AuthorizeDiscovery = func(context.Context, id.OrchestrationID) error {
		if !allowed {
			return denied
		}
		return nil
	}
	observer := recipientObserver(func(context.Context, a2a.RecipientRequest) (a2a.RecipientObservation, error) {
		t.Error("readiness observed a child")
		return a2a.RecipientObservation{}, denied
	})
	bus := recipientRecoveryBus(ctx, t, backend, first, observer, config, true)
	ready, err := bus.PendingDispatch(ctx)
	require.NoError(t, err)
	require.False(t, ready)
	_, err = replies.CommitReservedReply(ctx, first, "answer", true)
	require.NoError(t, err)
	ready, err = bus.PendingDispatch(ctx)
	require.NoError(t, err)
	require.True(t, ready)
	_, err = bus.Drain(ctx)
	require.NoError(t, err)
	ready, err = bus.PendingDispatch(ctx)
	require.NoError(t, err)
	require.False(t, ready)
	question, err := private.GetMessage(ctx, first.MessageID)
	require.NoError(t, err)
	deadline := time.Now().Add(-time.Minute)
	require.NoError(t, private.CreatePendingAsk(ctx, &a2a.PendingAsk{Entity: cortex.NewEntity(), Scope: cortex.ScopeFromContext(ctx), ReplyWith: "expired-private", ConversationID: question.ConversationID, MessageID: question.ID, AskerRunID: id.NewAgentRunID(), AskerAgent: "planner", ToolCallID: "ask", Expected: first.Receiver, Deadline: &deadline}))
	ready, err = bus.PendingDispatch(ctx)
	require.NoError(t, err)
	require.True(t, ready)
	allowed = false
	ready, err = bus.PendingDispatch(ctx)
	require.ErrorIs(t, err, denied)
	require.False(t, ready)
	_, err = bus.PendingDispatch(ctxWithScope("foreign"))
	require.ErrorIs(t, err, a2a.ErrRosterDenied)
}
