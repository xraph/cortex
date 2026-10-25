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
	"github.com/xraph/cortex/store/internal/assurance"
)

func testA2AAskResolution(t *testing.T, newStore func(*testing.T) store.Store) {
	ctx, private, resolutions, ask := prepareAskResolution(t, newStore(t))
	reply := a2a.AskReply{Complete: true, Replies: []a2a.AskReplyItem{{Performative: "inform", Sender: "worker", Content: "checked", ConversationID: ask.ConversationID.String()}}}
	sealed, err := resolutions.SealAskResolution(ctx, ask, reply)
	require.NoError(t, err)
	require.True(t, sealed.Fresh)
	require.Equal(t, ask.AskerRunID, sealed.Ask.AskerRunID)
	require.Contains(t, sealed.Payload, "checked")
	pending, err := private.ListPendingAsksByConversation(ctx, ask.ConversationID)
	require.NoError(t, err)
	require.Empty(t, pending)
	replay, err := resolutions.SealAskResolution(ctx, ask, reply)
	require.NoError(t, err)
	require.False(t, replay.Fresh)
	require.Equal(t, sealed.ResolutionID, replay.ResolutionID)
	reply.Complete = false
	_, err = resolutions.SealAskResolution(ctx, ask, reply)
	require.ErrorIs(t, err, a2a.ErrAskAlreadyClaimed)
	observed, err := resolutions.GetAskResolution(ctx, ask.ReplyWith)
	require.NoError(t, err)
	require.False(t, observed.Fresh)
	require.Equal(t, sealed.Payload, observed.Payload)
	_, err = resolutions.GetAskResolution(context.Background(), ask.ReplyWith)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = resolutions.GetAskResolution(ctxWithScope("other"), ask.ReplyWith)
	require.ErrorIs(t, err, a2a.ErrAskResolutionNotFound)
}

func prepareAskResolution(t *testing.T, backend store.Store) (context.Context, a2a.Store, a2a.AskResolutionStore, *a2a.PendingAsk) {
	t.Helper()
	ctx := ctxWithScope(id.NewMessageID().String(), "project")
	team := id.NewOrchestrationID()
	private, err := a2a.NewRoutingStore(backend, team)
	require.NoError(t, err)
	resolutions, ok := private.(a2a.AskResolutionStore)
	require.True(t, ok)
	bus, err := a2a.NewBus(a2a.BusConfig{Store: backend, Runner: routingRejectRunner{}, OrchestrationID: team, Synchronous: true})
	require.NoError(t, err)
	sent, err := bus.Ask(ctx, a2a.AskParams{SendParams: a2a.SendParams{Sender: a2a.Address{Agent: "planner"}, Receivers: []a2a.Address{{Agent: "worker"}}, Content: "evaluate"}, AskerRunID: id.NewAgentRunID(), ToolCallID: "ask-worker", Hold: true})
	require.NoError(t, err)
	ask, err := resolutions.ReadPendingAsk(ctx, sent.ReplyWith)
	require.NoError(t, err)
	return ctx, private, resolutions, ask
}

func resolutionReply(ask *a2a.PendingAsk, complete bool) a2a.AskReply {
	return a2a.AskReply{Complete: complete, Replies: []a2a.AskReplyItem{{Performative: "inform", Sender: "worker", Content: "checked", ConversationID: ask.ConversationID.String()}}}
}

func testA2AAskResolutionConcurrency(t *testing.T, newStore func(*testing.T) store.Store) {
	ctx, private, resolutions, ask := prepareAskResolution(t, newStore(t))
	var wg sync.WaitGroup
	results := make(chan a2a.AskResolution, 8)
	failures := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			outcome, err := resolutions.SealAskResolution(ctx, ask, resolutionReply(ask, i%2 == 0))
			results <- outcome
			failures <- err
		})
	}
	wg.Wait()
	close(results)
	close(failures)
	for err := range failures {
		if err != nil {
			require.ErrorIs(t, err, a2a.ErrAskAlreadyClaimed)
		}
	}
	observed, err := resolutions.GetAskResolution(ctx, ask.ReplyWith)
	require.NoError(t, err)
	fresh := 0
	for result := range results {
		if result.Fresh {
			fresh++
		}
		if result.ResolutionID != "" {
			require.Equal(t, observed.Payload, result.Payload)
		}
	}
	require.Equal(t, 1, fresh)
	_, err = private.ClaimPendingAsk(ctx, ask.ReplyWith)
	require.ErrorIs(t, err, a2a.ErrAskAlreadyClaimed)
}

func testA2AAskResolutionBoundaries(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, private, resolutions, ask := prepareAskResolution(t, backend)
	reply := resolutionReply(ask, true)
	changed := *ask
	changed.ToolCallID = "different"
	_, err := resolutions.SealAskResolution(ctx, &changed, reply)
	require.ErrorIs(t, err, a2a.ErrAskResolutionConflict)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = resolutions.SealAskResolution(cancelled, ask, reply)
	require.ErrorIs(t, err, context.Canceled)
	_, err = resolutions.SealAskResolution(context.Background(), ask, reply)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = resolutions.SealAskResolution(ctxWithScope("foreign"), ask, reply)
	require.ErrorIs(t, err, cortex.ErrInvalidScope)
	reply.Replies[0].Content = string([]byte{0xff})
	_, err = resolutions.SealAskResolution(ctx, ask, reply)
	require.ErrorIs(t, err, a2a.ErrAskResolutionConflict)
	pending, err := private.ListPendingAsksByConversation(ctx, ask.ConversationID)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	// An old claim with no retained outcome cannot authorize a new resume.
	_, err = private.ClaimPendingAsk(ctx, ask.ReplyWith)
	require.NoError(t, err)
	_, err = resolutions.SealAskResolution(ctx, ask, resolutionReply(ask, true))
	require.ErrorIs(t, err, a2a.ErrAskAlreadyClaimed)
	_, err = resolutions.GetAskResolution(ctx, ask.ReplyWith)
	require.ErrorIs(t, err, a2a.ErrAskResolutionNotFound)
}

type askResolutionFaultDriver struct {
	redirectID string
	assurance.Driver
	transaction assurance.AskResolutionDriver
	rollback    bool
	loseAck     bool
	cancel      context.CancelFunc
}

func (d askResolutionFaultDriver) Select(ctx context.Context, query assurance.Query) ([]assurance.Record, error) {
	if d.redirectID != "" {
		query.ID = d.redirectID
	}
	return d.Driver.Select(ctx, query)
}

func (d askResolutionFaultDriver) ReadPendingAsk(ctx context.Context, token string) (*a2a.PendingAsk, error) {
	return d.transaction.ReadPendingAsk(ctx, token)
}
func (d askResolutionFaultDriver) TransactAskResolution(ctx context.Context, token string, callback func(context.Context, assurance.AskResolutionTransaction) error) error {
	err := d.transaction.TransactAskResolution(ctx, token, func(tc context.Context, tx assurance.AskResolutionTransaction) error {
		if callbackErr := callback(tc, tx); callbackErr != nil {
			return callbackErr
		}
		if d.cancel != nil {
			d.cancel()
			return tc.Err()
		}
		if d.rollback {
			return errors.New("injected rollback")
		}
		return nil
	})
	if err == nil && d.loseAck {
		return errors.New("lost commit acknowledgment")
	}
	return err
}

type resolutionStore struct {
	store.Store
	a2a.AskResolutionStore
}

func (*resolutionStore) SupportsOrchestrationRouting() bool { return true }

// A2AAskResolutionTransactions tests native rollback and ambiguous acknowledgments.
func A2AAskResolutionTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	transaction, ok := driver.(assurance.AskResolutionDriver)
	require.True(t, ok)
	for _, mode := range []string{"rollback", "lost_ack", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, private, resolutions, ask := prepareAskResolution(t, backend)
			attemptCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			fault := askResolutionFaultDriver{Driver: driver, transaction: transaction, rollback: mode == "rollback", loseAck: mode == "lost_ack"}
			if mode == "cancelled" {
				fault.cancel = cancel
			}
			wrapped := &resolutionStore{Store: backend, AskResolutionStore: assurance.New(fault)}
			team, err := id.ParseOrchestrationID(ask.OrchestrationID)
			require.NoError(t, err)
			routed, err := a2a.NewRoutingStore(wrapped, team)
			require.NoError(t, err)
			faulty, ok := routed.(a2a.AskResolutionStore)
			require.True(t, ok)
			result, err := faulty.SealAskResolution(attemptCtx, ask, resolutionReply(ask, true))
			require.Error(t, err)
			require.Zero(t, result)
			pending, err := private.ListPendingAsksByConversation(ctx, ask.ConversationID)
			require.NoError(t, err)
			if mode == "lost_ack" {
				require.Empty(t, pending)
			} else {
				require.Len(t, pending, 1)
			}
			result, err = resolutions.SealAskResolution(ctx, ask, resolutionReply(ask, true))
			require.NoError(t, err)
			require.Equal(t, mode != "lost_ack", result.Fresh)
			require.Contains(t, result.Payload, "checked")
			if mode == "lost_ack" {
				redirected := assurance.New(askResolutionFaultDriver{Driver: driver, transaction: transaction, redirectID: result.ResolutionID})
				misrouted, routeErr := a2a.NewRoutingStore(&resolutionStore{Store: backend, AskResolutionStore: redirected}, team)
				require.NoError(t, routeErr)
				reader, supported := misrouted.(a2a.AskResolutionStore)
				require.True(t, supported)
				_, readErr := reader.GetAskResolution(ctx, "another-token")
				require.ErrorIs(t, readErr, a2a.ErrAskResolutionConflict, "a receipt must bind the requested correlation token")
			}
		})
	}
}

func testA2AAskResolutionHistory(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, _, resolutions, ask := prepareAskResolution(t, backend)
	_, err := resolutions.SealAskResolution(ctx, ask, resolutionReply(ask, true))
	require.NoError(t, err)
	team, err := id.ParseOrchestrationID(ask.OrchestrationID)
	require.NoError(t, err)
	bus, err := a2a.NewBus(a2a.BusConfig{Store: backend, Runner: routingRejectRunner{}, OrchestrationID: team, Synchronous: true})
	require.NoError(t, err)
	for range 2 {
		sent, sendErr := bus.Ask(ctx, a2a.AskParams{SendParams: a2a.SendParams{Sender: a2a.Address{Agent: "planner"}, Receivers: []a2a.Address{{Agent: "worker"}}, Content: "evaluate"}, AskerRunID: id.NewAgentRunID(), ToolCallID: "ask-worker", Hold: true})
		require.NoError(t, sendErr)
		pending, readErr := resolutions.ReadPendingAsk(ctx, sent.ReplyWith)
		require.NoError(t, readErr)
		_, sealErr := resolutions.SealAskResolution(ctx, pending, resolutionReply(pending, true))
		require.NoError(t, sealErr)
	}
	page, err := resolutions.ListAskResolutions(ctx, "", 1)
	require.NoError(t, err)
	require.Len(t, page.Resolutions, 1)
	require.NotEmpty(t, page.Next)
	seen := map[string]bool{page.Resolutions[0].ResolutionID: true}
	for page.Next != "" {
		page, err = resolutions.ListAskResolutions(ctx, page.Next, 1)
		require.NoError(t, err)
		require.Len(t, page.Resolutions, 1)
		require.False(t, seen[page.Resolutions[0].ResolutionID])
		seen[page.Resolutions[0].ResolutionID] = true
	}
	require.Len(t, seen, 3)
	require.False(t, page.Resolutions[0].Fresh)
	global, err := a2a.NewRoutingStore(backend, id.Nil)
	require.NoError(t, err)
	globalResolutions, ok := global.(a2a.AskResolutionStore)
	require.True(t, ok)
	page, err = globalResolutions.ListAskResolutions(ctx, "", 1)
	require.NoError(t, err)
	require.Empty(t, page.Resolutions)
	_, err = globalResolutions.GetAskResolution(ctx, ask.ReplyWith)
	require.ErrorIs(t, err, a2a.ErrAskResolutionNotFound)
	_, err = globalResolutions.ListAskResolutions(ctx, page.Next+"invalid", 1)
	require.ErrorIs(t, err, a2a.ErrAskResolutionConflict)
	_, err = resolutions.ListAskResolutions(ctx, "", 257)
	require.ErrorIs(t, err, a2a.ErrAskResolutionConflict)
	_, err = resolutions.ListAskResolutions(context.Background(), "", 1)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	// Prefix scope access must not enumerate another project's resume payloads.
	scope := cortex.ScopeFromContext(ctx)
	parent := cortex.WithScope(ctx, cortex.Scope{Levels: scope.Levels[:1]})
	page, err = resolutions.ListAskResolutions(parent, "", 1)
	require.NoError(t, err)
	require.Empty(t, page.Resolutions)
}

type outcomeResumer struct {
	calls int
	fail  bool
}

func (r *outcomeResumer) ResumeAgentReply(context.Context, id.AgentRunID, string, string) error {
	r.calls++
	if r.fail {
		return errors.New("ambiguous resume")
	}
	return nil
}

type resolutionAckStore struct{ a2a.AskResolutionStore }

func (s resolutionAckStore) SealAskResolution(ctx context.Context, ask *a2a.PendingAsk, reply a2a.AskReply) (a2a.AskResolution, error) {
	_, err := s.AskResolutionStore.SealAskResolution(ctx, ask, reply)
	if err != nil {
		return a2a.AskResolution{}, err
	}
	return a2a.AskResolution{}, errors.New("lost seal acknowledgment")
}

func testA2AAskResolutionBus(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"reply", "timeout", "cancel", "lost_ack", "resume_error"} {
		t.Run(mode, func(t *testing.T) {
			backend := newStore(t)
			ctx, _, resolutions, ask := prepareAskResolution(t, backend)
			team, err := id.ParseOrchestrationID(ask.OrchestrationID)
			require.NoError(t, err)
			native, ok := backend.(a2a.AskResolutionStore)
			require.True(t, ok)
			wrapper := &resolutionStore{Store: backend, AskResolutionStore: native}
			if mode == "lost_ack" {
				wrapper.AskResolutionStore = resolutionAckStore{native}
			}
			resumer := &outcomeResumer{fail: mode == "resume_error"}
			cfg := a2a.BusConfig{Store: wrapper, Runner: routingRejectRunner{}, Resumer: resumer, OrchestrationID: team, Synchronous: true}
			if mode == "timeout" {
				cfg.Clock = rosterClock{at: time.Now().Add(time.Hour)}
			}
			bus, err := a2a.NewBus(cfg)
			require.NoError(t, err)
			params := a2a.SendParams{Sender: ask.Expected, Receivers: []a2a.Address{{Agent: ask.AskerAgent}}, Performative: a2a.Inform, Content: "answer", ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith}
			switch mode {
			case "timeout":
				_, err = bus.SweepExpiredAsks(ctx)
			case "cancel":
				params.Performative = a2a.Cancel
				_, err = bus.Send(ctx, params)
				require.NoError(t, err)
				_, err = bus.Drain(ctx)
			default:
				_, err = bus.Send(ctx, params)
			}
			if mode == "lost_ack" || mode == "resume_error" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			wantCalls := 1
			if mode == "lost_ack" {
				wantCalls = 0
			}
			require.Equal(t, wantCalls, resumer.calls)
			observed, err := resolutions.GetAskResolution(ctx, ask.ReplyWith)
			require.NoError(t, err)
			require.False(t, observed.Fresh)
			require.NotEmpty(t, observed.Payload)
			if mode == "lost_ack" || mode == "resume_error" {
				cfg.Store = backend
				fresh, createErr := a2a.NewBus(cfg)
				require.NoError(t, createErr)
				_, err = fresh.Send(ctx, params)
				require.NoError(t, err)
				require.Equal(t, wantCalls, resumer.calls, "observed resolution is not a resume retry grant")
			}
		})
	}
}

type resolutionReadFaultStore struct{ *resolutionStore }

func (*resolutionReadFaultStore) GetMessage(context.Context, id.MessageID) (*a2a.Envelope, error) {
	return nil, errors.New("source message unavailable")
}

func testA2AAskResolutionReadFailure(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx, private, resolutions, ask := prepareAskResolution(t, backend)
	team, err := id.ParseOrchestrationID(ask.OrchestrationID)
	require.NoError(t, err)
	native, ok := backend.(a2a.AskResolutionStore)
	require.True(t, ok)
	wrapped := &resolutionReadFaultStore{&resolutionStore{Store: backend, AskResolutionStore: native}}
	resumer := &outcomeResumer{}
	bus, err := a2a.NewBus(a2a.BusConfig{Store: wrapped, Runner: routingRejectRunner{}, Resumer: resumer, OrchestrationID: team, Synchronous: true, Clock: rosterClock{at: time.Now().Add(time.Hour)}})
	require.NoError(t, err)
	_, err = bus.SweepExpiredAsks(ctx)
	require.Error(t, err)
	require.Zero(t, resumer.calls)
	pending, err := private.ListPendingAsksByConversation(ctx, ask.ConversationID)
	require.NoError(t, err)
	require.Len(t, pending, 1, "unavailable source evidence must not consume the pending ask")
	_, err = resolutions.GetAskResolution(ctx, ask.ReplyWith)
	require.ErrorIs(t, err, a2a.ErrAskResolutionNotFound)
}
