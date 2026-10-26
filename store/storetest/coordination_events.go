package storetest

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

// CoordinationEvents verifies durable import, stable delivery identities and
// current authority using actual accepted artifacts and their source journals.
func CoordinationEvents(t *testing.T, s store.Store) {
	ctx, requests := artifactBoardFixture(t, s)
	board := resultCapability[coordination.ArtifactBoardStore](t, s)
	first, err := board.WriteArtifactBoard(ctx, requests[0])
	require.NoError(t, err)
	events := resultCapability[coordination.EventStore](t, s)
	page := coordination.EventPage{Scope: first.Request.Scope, PlanID: first.Request.PlanID, Limit: 256}
	initial, err := events.ListCoordinationEvents(ctx, page)
	require.NoError(t, err)
	require.Len(t, initial, 1, "board intent must commit atomically with the head")
	require.Equal(t, "artifact_board_published", initial[0].Type)
	for _, source := range coordination.EventSources() {
		after := ""
		for pages := 0; ; pages++ {
			require.Less(t, pages, 32)
			imported, importErr := events.ImportCoordinationEvents(ctx, coordination.EventImport{Scope: page.Scope, PlanID: page.PlanID, Source: source, After: after, Limit: 1})
			require.NoError(t, importErr)
			if imported.Next == "" {
				break
			}
			require.NotEqual(t, after, imported.Next)
			after = imported.Next
		}
	}
	retained, err := events.ListCoordinationEvents(ctx, page)
	require.NoError(t, err)
	require.Greater(t, len(retained), 2)
	found := false
	accepted, err := resultCapability[coordination.TaskAcceptanceStore](t, s).GetTaskAcceptance(ctx, requests[0].AcceptanceID)
	require.NoError(t, err)
	for _, event := range retained {
		require.NoError(t, event.Validate())
		if event.EventID == accepted.Outbox.EventID.String() {
			found = true
			require.Equal(t, accepted.Digest, event.SourceDigest)
		}
	}
	require.True(t, found, "existing outbox identity must survive import")
	for _, source := range coordination.EventSources() {
		after := ""
		for {
			imported, importErr := events.ImportCoordinationEvents(ctx, coordination.EventImport{Scope: page.Scope, PlanID: page.PlanID, Source: source, After: after, Limit: 256})
			require.NoError(t, importErr)
			require.Zero(t, imported.Imported)
			if imported.Next == "" {
				break
			}
			after = imported.Next
		}
	}
	replay, err := events.ListCoordinationEvents(ctx, page)
	require.NoError(t, err)
	require.Equal(t, retained, replay)
	t.Run("lost acknowledgment and restart", func(t *testing.T) {
		authority := &eventFixtureAuthority{}
		sink := &eventFixtureSink{lost: true, receipts: make(map[string]coordination.Delivery), calls: make(map[string]int)}
		config := eventFixtureConfig(events, page, authority, sink, "fixture")
		managed, createErr := engine.New(engine.WithStore(s), engine.WithCoordinationEvents(config), engine.WithSuspensionTTL(0))
		require.NoError(t, createErr)
		require.NoError(t, managed.Start(ctx))
		t.Cleanup(func() { require.NoError(t, managed.Stop(context.Background())) })
		require.Eventually(t, func() bool {
			receipt, readErr := events.GetCoordinationDelivery(ctx, retained[len(retained)-1], "fixture")
			return readErr == nil && receipt != nil
		}, 10*time.Second, 10*time.Millisecond)
		require.NoError(t, managed.Stop(context.Background()))
		status, enabled := managed.CoordinationEventStatus()
		require.True(t, enabled)
		require.Positive(t, status.Failures)
		sink.mu.Lock()
		require.Len(t, sink.receipts, len(retained))
		require.GreaterOrEqual(t, sink.calls[initial[0].EventID], 2)
		before := sink.calls[initial[0].EventID]
		sink.mu.Unlock()
		restarted, createErr := coordination.NewEventService(config)
		require.NoError(t, createErr)
		require.NoError(t, restarted.Start(ctx))
		time.Sleep(60 * time.Millisecond)
		require.NoError(t, restarted.Stop(context.Background()))
		sink.mu.Lock()
		require.Equal(t, before, sink.calls[initial[0].EventID])
		sink.mu.Unlock()
	})
	for _, mode := range []string{"revoked after read", "timeout", "drain"} {
		t.Run(mode, func(t *testing.T) {
			authority := &eventFixtureAuthority{}
			sink := &eventFixtureSink{receipts: make(map[string]coordination.Delivery), calls: make(map[string]int)}
			storage := events
			if mode == "revoked after read" {
				storage = eventReadRevocation{EventStore: events, revoked: &authority.revoked}
			}
			if mode == "timeout" || mode == "drain" {
				sink.entered = make(chan struct{})
				if mode == "drain" {
					sink.release = make(chan struct{})
				}
			}
			config := eventFixtureConfig(storage, page, authority, sink, mode)
			if mode == "timeout" {
				config.PageTimeout = 50 * time.Millisecond
			}
			service, createErr := coordination.NewEventService(config)
			require.NoError(t, createErr)
			require.NoError(t, service.Start(ctx))
			t.Cleanup(func() { require.NoError(t, service.Stop(context.Background())) })
			if mode == "drain" {
				select {
				case <-sink.entered:
				case <-time.After(10 * time.Second):
					t.Fatal("sink did not start")
				}
				stopping, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
				require.ErrorIs(t, service.Stop(stopping), context.DeadlineExceeded)
				cancel()
				require.True(t, service.Status().Stopping)
				close(sink.release)
			} else {
				require.Eventually(t, func() bool { return service.Status().Failures > 0 }, 10*time.Second, 10*time.Millisecond)
			}
			require.NoError(t, service.Stop(context.Background()))
			if mode == "revoked after read" {
				sink.mu.Lock()
				require.Empty(t, sink.calls)
				sink.mu.Unlock()
			}
			receipt, readErr := events.GetCoordinationDelivery(ctx, initial[0], mode)
			require.NoError(t, readErr)
			require.Nil(t, receipt)
		})
	}
}
func eventFixtureConfig(storage coordination.EventStore, page coordination.EventPage, authority *eventFixtureAuthority, sink *eventFixtureSink, name string) coordination.EventServiceConfig {
	return coordination.EventServiceConfig{Store: storage, Discovery: adoptionPlanSource(func(context.Context, coordination.TaskPlanListRequest) (coordination.TaskPlanPage, error) {
		return coordination.TaskPlanPage{Plans: []coordination.ScheduledPlan{{Scope: page.Scope, PlanID: page.PlanID}}}, nil
	}), Authority: authority, Sink: sink, SinkID: name, Interval: 10 * time.Millisecond, PageSize: 2, MaxInFlight: 1}
}

type eventFixtureAuthority struct{ revoked atomic.Bool }

func (a *eventFixtureAuthority) AuthorizeCoordinationScan(context.Context, id.ID, string) error {
	if a.revoked.Load() {
		return errAssignmentInjected
	}
	return nil
}
func (a *eventFixtureAuthority) WithCoordinationDeliveryFence(_ context.Context, r coordination.DeliveryRequest, next func(coordination.DeliveryRequest) error) error {
	return next(r)
}
func (a *eventFixtureAuthority) AuthorizeCoordinationDelivery(context.Context, coordination.DeliveryRequest) error {
	if a.revoked.Load() {
		return errAssignmentInjected
	}
	return nil
}

type eventFixtureSink struct {
	mu               sync.Mutex
	lost             bool
	receipts         map[string]coordination.Delivery
	calls            map[string]int
	entered, release chan struct{}
	once             sync.Once
}

func (s *eventFixtureSink) DeliverCoordinationEvent(ctx context.Context, request coordination.DeliveryRequest) (coordination.Delivery, error) {
	s.mu.Lock()
	s.calls[request.Event.EventID]++
	s.mu.Unlock()
	if s.entered != nil {
		s.once.Do(func() { close(s.entered) })
		<-ctx.Done()
		if s.release != nil {
			<-s.release
		}
		return coordination.Delivery{}, ctx.Err()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	receipt, exists := s.receipts[request.Event.EventID]
	if !exists {
		receipt = coordination.Delivery{Event: request.Event, SinkID: request.SinkID, Reference: "durable:" + request.Event.EventID}
		s.receipts[request.Event.EventID] = receipt
	}
	if s.lost {
		s.lost = false
		return coordination.Delivery{}, errAssignmentInjected
	}
	return receipt, nil
}

type eventReadRevocation struct {
	coordination.EventStore
	revoked *atomic.Bool
}

func (s eventReadRevocation) ListCoordinationEvents(ctx context.Context, page coordination.EventPage) ([]coordination.Event, error) {
	events, err := s.EventStore.ListCoordinationEvents(ctx, page)
	s.revoked.Store(true)
	return events, err
}
