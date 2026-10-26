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
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/suspension"
)

func prepareHeldRelease(t *testing.T, backend store.Store, receivers ...a2a.Address) (context.Context, *a2a.Bus, a2a.Store, a2a.HeldAskStore, a2a.HeldAskRequest) {
	t.Helper()
	if len(receivers) == 0 {
		receivers = []a2a.Address{{Agent: "worker"}}
	}
	ctx := ctxWithScope(id.NewMessageID().String(), "project")
	current, paused := mustSuspendRun(t, backend, ctx, nil)
	team := id.NewOrchestrationID()
	current.Provenance.OrchestrationID = team.String()
	require.NoError(t, backend.UpdateRun(ctx, current))
	current, err := backend.GetRun(ctx, current.ID)
	require.NoError(t, err)
	bus, err := a2a.NewBus(a2a.BusConfig{Store: backend, Runner: routingRejectRunner{}, OrchestrationID: team, Synchronous: true})
	require.NoError(t, err)
	sent, err := bus.Ask(ctx, a2a.AskParams{SendParams: a2a.SendParams{Sender: a2a.Address{Agent: "planner"}, Receivers: receivers, Content: "evaluate", OriginRunID: current.ID}, AskerRunID: current.ID, ToolCallID: paused.Pending[0].ID, Hold: true, DurableHold: true})
	require.NoError(t, err)
	require.NoError(t, backend.DeleteSuspension(ctx, current.ID))
	paused.Reason = suspension.ReasonAgentReply
	paused.Pending[0].Name, paused.Pending[0].ReplyWith = "agent_ask", sent.ReplyWith
	require.NoError(t, backend.CreateSuspension(ctx, paused))
	private, err := a2a.NewRoutingStore(backend, team)
	require.NoError(t, err)
	writer, ok := private.(a2a.HeldAskStore)
	require.True(t, ok)
	return ctx, bus, private, writer, a2a.HeldAskRequest{RunID: current.ID, SuspensionID: paused.ID, RunRevision: current.Revision, ReplyWith: sent.ReplyWith}
}

func testA2AHeldRelease(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, bus, private, writer, req := prepareHeldRelease(t, backend)
	ready, err := bus.HeldAskPending(ctx, req.ReplyWith)
	require.NoError(t, err)
	require.True(t, ready)
	stale := req
	stale.SuspensionID = id.NewSuspensionID()
	_, err = writer.ReleaseHeldAsk(ctx, stale)
	require.ErrorIs(t, err, cortex.ErrNotSuspended)
	stale = req
	stale.RunRevision++
	_, err = writer.ReleaseHeldAsk(ctx, stale)
	require.ErrorIs(t, err, cortex.ErrRunConflict)
	_, err = writer.ReleaseHeldAsk(ctxWithScope("other"), req)
	require.Error(t, err)
	queued, err := private.ListQueuedDeliveries(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, queued)
	receipt, err := bus.ReleasePausedAsk(ctx, req)
	require.NoError(t, err)
	require.True(t, receipt.Fresh)
	require.Len(t, receipt.Deliveries, 1)
	queued, err = private.ListQueuedDeliveries(ctx, 10)
	require.NoError(t, err)
	require.Len(t, queued, 1)
	require.Equal(t, receipt.Deliveries[0], queued[0].ID)
	ready, err = bus.HeldAskPending(ctx, req.ReplyWith)
	require.NoError(t, err)
	require.False(t, ready)
	// Observing or retrying a committed release must not require the old pause.
	current, err := backend.GetRun(ctx, req.RunID)
	require.NoError(t, err)
	current.State = run.StateCompleted
	require.NoError(t, backend.UpdateRun(ctx, current))
	require.NoError(t, backend.DeleteSuspension(ctx, current.ID))
	again, err := writer.ReleaseHeldAsk(ctx, req)
	require.NoError(t, err)
	require.False(t, again.Fresh)
	require.Equal(t, receipt.Deliveries, again.Deliveries)
	_, err = writer.ReleaseHeldAsk(ctx, stale)
	require.ErrorIs(t, err, a2a.ErrHeldAskConflict)
}

func testA2AHeldReleaseConcurrency(t *testing.T, newStore func(*testing.T) store.Store) {
	ctx, _, private, writer, req := prepareHeldRelease(t, newStore(t))
	var wg sync.WaitGroup
	outcomes := make(chan a2a.HeldAskReceipt, 4)
	for range 4 {
		wg.Go(func() { result, err := writer.ReleaseHeldAsk(ctx, req); require.NoError(t, err); outcomes <- result })
	}
	wg.Wait()
	close(outcomes)
	fresh := 0
	var delivery id.DeliveryID
	for result := range outcomes {
		if result.Fresh {
			fresh++
		}
		require.Len(t, result.Deliveries, 1)
		if delivery.IsNil() {
			delivery = result.Deliveries[0]
		}
		require.Equal(t, delivery, result.Deliveries[0])
	}
	require.Equal(t, 1, fresh)
	queued, err := private.ListQueuedDeliveries(ctx, 10)
	require.NoError(t, err)
	require.Len(t, queued, 1)
}

// A2AHeldReleaseTransactions checks rollback and lost commit acknowledgments.
func A2AHeldReleaseTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	for _, lostAck := range []bool{false, true} {
		ctx, _, private, writer, req := prepareHeldRelease(t, backend, a2a.Address{Agent: "worker"}, a2a.Address{Agent: "reviewer"})
		// The routing store carries a private team, so recover its exact partition.
		askReader, available := private.(a2a.AskResolutionStore)
		require.True(t, available)
		ask, err := askReader.ReadPendingAsk(ctx, req.ReplyWith)
		require.NoError(t, err)
		team, err := id.ParseOrchestrationID(ask.OrchestrationID)
		require.NoError(t, err)
		failure := errors.New("injected held question commit error")
		var faulty assurance.Driver = failingRunDriver{Driver: driver, transaction: transaction, failure: failure}
		if lostAck {
			faulty = privateLostReplyDriver{Driver: driver, transaction: transaction, failure: failure}
		}
		wrapped, err := a2a.NewRoutingStore(&heldReleaseStore{Store: backend, HeldAskStore: assurance.New(faulty)}, team)
		require.NoError(t, err)
		releaser, ok := wrapped.(a2a.HeldAskStore)
		require.True(t, ok)
		result, err := releaser.ReleaseHeldAsk(ctx, req)
		require.ErrorIs(t, err, failure)
		require.Empty(t, result.Deliveries)
		queued, err := private.ListQueuedDeliveries(ctx, 10)
		require.NoError(t, err)
		if lostAck {
			require.Len(t, queued, 2)
		} else {
			require.Empty(t, queued)
		}
		receipt, err := writer.ReleaseHeldAsk(ctx, req)
		require.NoError(t, err)
		require.Equal(t, !lostAck, receipt.Fresh)
		queued, err = private.ListQueuedDeliveries(ctx, 10)
		require.NoError(t, err)
		require.Len(t, queued, 2)
	}
}

type heldReleaseStore struct {
	store.Store
	a2a.HeldAskStore
}

func (*heldReleaseStore) SupportsOrchestrationRouting() bool { return true }

func testA2AHeldReleaseBoundaries(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"expired_pause", "wrong_call", "claimed_question", "closed_conversation", "scope_prefix", "wrong_team", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			backend := newStore(t)
			ctx, _, private, writer, req := prepareHeldRelease(t, backend)
			attempt := ctx
			switch mode {
			case "expired_pause", "wrong_call":
				pause, err := backend.GetSuspension(ctx, req.RunID)
				require.NoError(t, err)
				require.NoError(t, backend.DeleteSuspension(ctx, req.RunID))
				if mode == "expired_pause" {
					pause.ExpiresAt = pastDeadline()
				} else {
					pause.Pending[0].ID = "changed"
				}
				require.NoError(t, backend.CreateSuspension(ctx, pause))
			case "claimed_question":
				_, err := private.ClaimPendingAsk(ctx, req.ReplyWith)
				require.NoError(t, err)
			case "closed_conversation":
				reader, available := private.(a2a.AskResolutionStore)
				require.True(t, available)
				ask, err := reader.ReadPendingAsk(ctx, req.ReplyWith)
				require.NoError(t, err)
				conversation, err := private.GetConversation(ctx, ask.ConversationID)
				require.NoError(t, err)
				conversation.Status = a2a.StatusClosed
				require.NoError(t, private.UpdateConversation(ctx, conversation))
			case "scope_prefix":
				scope := cortex.ScopeFromContext(ctx)
				scope.Levels = scope.Levels[:1]
				attempt = cortex.WithScope(ctx, scope)
			case "wrong_team":
				other, err := a2a.NewRoutingStore(backend, id.NewOrchestrationID())
				require.NoError(t, err)
				var available bool
				writer, available = other.(a2a.HeldAskStore)
				require.True(t, available)
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				attempt = cancelled
			}
			receipt, err := writer.ReleaseHeldAsk(attempt, req)
			require.Error(t, err)
			require.Zero(t, receipt)
			queued, err := private.ListQueuedDeliveries(ctx, 10)
			require.NoError(t, err)
			require.Empty(t, queued)
			current, err := backend.GetRun(ctx, req.RunID)
			require.NoError(t, err)
			require.Equal(t, run.StatePaused, current.State)
			require.Equal(t, req.RunRevision, current.Revision)
		})
	}
}

func testA2AHeldReleaseLegacy(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, bus, private, writer, req := prepareHeldRelease(t, backend)
	sent, err := bus.Ask(ctx, a2a.AskParams{SendParams: a2a.SendParams{Sender: a2a.Address{Agent: "planner"}, Receivers: []a2a.Address{{Agent: "worker"}}, Content: "legacy question", OriginRunID: req.RunID}, AskerRunID: req.RunID, ToolCallID: "legacy-call", Hold: true})
	require.NoError(t, err)
	pause, err := backend.GetSuspension(ctx, req.RunID)
	require.NoError(t, err)
	require.NoError(t, backend.DeleteSuspension(ctx, req.RunID))
	pause.Pending[0].ID, pause.Pending[0].ReplyWith = "legacy-call", sent.ReplyWith
	require.NoError(t, backend.CreateSuspension(ctx, pause))
	req.ReplyWith = sent.ReplyWith
	pending, err := bus.HeldAskPending(ctx, req.ReplyWith)
	require.NoError(t, err)
	require.False(t, pending, "old questions cannot prove that a legacy dispatcher never queued them")
	_, err = writer.ReleaseHeldAsk(ctx, req)
	require.ErrorIs(t, err, a2a.ErrHeldAskConflict)
	queued, err := private.ListQueuedDeliveries(ctx, 10)
	require.NoError(t, err)
	require.Empty(t, queued)
}
