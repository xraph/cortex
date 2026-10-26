package coordination

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/planning"
)

// DeliveryRequest pins one immutable private event and sink.
type DeliveryRequest struct {
	Event  Event  `json:"event"`
	SinkID string `json:"sink_id"`
}

// Freeze detaches metadata before passing it to host ports.
func (r DeliveryRequest) Freeze() (DeliveryRequest, string, error) {
	if r.Event.Validate() != nil || !handoffText(r.SinkID, 512) {
		return DeliveryRequest{}, "", planning.ErrInvalid
	}
	data, err := json.Marshal(r)
	if err != nil {
		return DeliveryRequest{}, "", err
	}
	var frozen DeliveryRequest
	if json.Unmarshal(data, &frozen) != nil {
		return DeliveryRequest{}, "", planning.ErrInvalid
	}
	return frozen, evidence.Digest(data), nil
}

// EventAuthority fences current source-read and sink-delivery grants.
// Scan categories ending in history authorize exact-plan claim traversal and its
// retained child metadata. Delivery must additionally check the exact SourceKind,
// SourceID and SourceDigest. Historical events never imply current work authority.
type EventAuthority interface {
	AuthorizeCoordinationScan(context.Context, id.ID, string) error
	WithCoordinationDeliveryFence(context.Context, DeliveryRequest, func(DeliveryRequest) error) error
	AuthorizeCoordinationDelivery(context.Context, DeliveryRequest) error
}

// EventSink durably deduplicates scope, PlanID, EventID and Digest within SinkID.
// It must authenticate its transport and returned receipt. Repeated calls with
// the same identity must return the same reference, including after lost acks.
// An uncertain outcome returns an error; never report a speculative acknowledgment.
type EventSink interface {
	DeliverCoordinationEvent(context.Context, DeliveryRequest) (Delivery, error)
}

// EventServiceConfig connects retained source intents to one bounded
// private metadata sink. Callbacks must honor cancellation; shutdown owns them
// until they return. PageSize defaults32, PageTimeout30s, tracked plans1024.
type EventServiceConfig struct {
	Store            EventStore
	Discovery        TaskPlanSource
	Authority        EventAuthority
	Sink             EventSink
	SinkID           string
	PageSize         int
	PageTimeout      time.Duration
	MaxTrackedPlans  int
	Interval         time.Duration
	DiscoveryTimeout time.Duration
	MaxInFlight      int
	MaxPerScope      int
	ReportError      func(error)
}

// EventService imports immutable outboxes and delivers metadata with
// stable identities. It never dispatches work or exports artifact contents.
type EventService struct{ service *TaskService }
type coordinationEventCursor struct {
	source    int
	after     string
	delivered int64
}
type coordinationEventWorker struct {
	config  EventServiceConfig
	mu      sync.Mutex
	cursors map[string]coordinationEventCursor
}

// NewEventService validates finite bounds without starting polling.
func NewEventService(config EventServiceConfig) (*EventService, error) {
	if hostport.IsNil(config.Store) || hostport.IsNil(config.Discovery) || hostport.IsNil(config.Authority) || hostport.IsNil(config.Sink) || !handoffText(config.SinkID, 512) {
		return nil, ErrInvalidScheduler
	}
	if config.PageSize == 0 {
		config.PageSize = 32
	}
	if config.PageTimeout == 0 {
		config.PageTimeout = 30 * time.Second
	}
	if config.MaxTrackedPlans == 0 {
		config.MaxTrackedPlans = 1024
	}
	if config.PageSize < 1 || config.PageSize > 256 || config.PageTimeout < time.Millisecond || config.PageTimeout > 30*time.Second || config.MaxTrackedPlans < 1 || config.MaxTrackedPlans > 4096 {
		return nil, ErrInvalidScheduler
	}
	worker := &coordinationEventWorker{config: config, cursors: make(map[string]coordinationEventCursor)}
	service, err := newIdentityOperation(planning.PrefixPlan, TaskServiceConfig{Discovery: config.Discovery, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, MaxInFlight: config.MaxInFlight, ReportError: config.ReportError, Scheduler: SchedulerConfig{MaxPerScope: config.MaxPerScope}}, config.PageTimeout, worker.tick)
	if err != nil {
		return nil, err
	}
	return &EventService{service: service}, nil
}
func (w *coordinationEventWorker) tick(ctx context.Context, planID id.ID) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	key := scope.Canonical() + "\x00" + planID.String()
	w.mu.Lock()
	cursor, exists := w.cursors[key]
	if !exists && len(w.cursors) >= w.config.MaxTrackedPlans {
		w.mu.Unlock()
		return ErrInvalidScheduler
	}
	w.cursors[key] = cursor
	w.mu.Unlock()
	defer func() {
		w.mu.Lock()
		if cursor.source == 0 && cursor.after == "" && cursor.delivered == 0 {
			delete(w.cursors, key)
		} else {
			w.cursors[key] = cursor
		}
		w.mu.Unlock()
	}()
	c := w.config
	sources := EventSources()
	source := sources[cursor.source]
	if operationErr := c.Authority.AuthorizeCoordinationScan(ctx, planID, source); operationErr != nil {
		return operationErr
	}
	if operationErr := ctx.Err(); operationErr != nil {
		return operationErr
	}
	page, err := c.Store.ImportCoordinationEvents(ctx, EventImport{Scope: scope, PlanID: planID, Source: source, After: cursor.after, Limit: c.PageSize})
	if err != nil {
		return err
	}
	if len(page.Next) > 4096 || page.Imported < 0 || page.Imported > 2*c.PageSize {
		return planning.ErrInvalid
	}
	if operationErr := c.Authority.AuthorizeCoordinationScan(ctx, planID, source); operationErr != nil {
		return operationErr
	}
	if operationErr := ctx.Err(); operationErr != nil {
		return operationErr
	}
	cursor.after = page.Next
	if page.Next == "" {
		cursor.source = (cursor.source + 1) % len(sources)
	}
	if operationErr := c.Authority.AuthorizeCoordinationScan(ctx, planID, "coordination_event"); operationErr != nil {
		return operationErr
	}
	events, err := c.Store.ListCoordinationEvents(ctx, EventPage{Scope: scope, PlanID: planID, After: cursor.delivered, Limit: c.PageSize})
	if err != nil {
		return err
	}
	if len(events) > c.PageSize {
		return planning.ErrInvalid
	}
	if operationErr := c.Authority.AuthorizeCoordinationScan(ctx, planID, "coordination_event"); operationErr != nil {
		return operationErr
	}
	if operationErr := ctx.Err(); operationErr != nil {
		return operationErr
	}
	for _, event := range events {
		if event.Validate() != nil || event.PlanID != planID || event.Scope.Canonical() != scope.Canonical() || event.Sequence != cursor.delivered+1 {
			return planning.ErrConflict
		}
		if operationErr := deliverCoordinationEvent(ctx, c, event); operationErr != nil {
			return operationErr
		}
		cursor.delivered = event.Sequence
	}
	if len(events) < c.PageSize {
		cursor.delivered = 0
	}
	return ctx.Err()
}
func deliverCoordinationEvent(ctx context.Context, c EventServiceConfig, event Event) error {
	request := DeliveryRequest{Event: event, SinkID: c.SinkID}
	guard := taskAuthority[DeliveryRequest]{operation: "coordination event delivery", invalid: planning.ErrInvalid, conflict: planning.ErrConflict, scope: func(r DeliveryRequest) cortex.Scope { return r.Event.Scope }, fence: c.Authority.WithCoordinationDeliveryFence, authorize: c.Authority.AuthorizeCoordinationDelivery}
	_, err := withTaskAuthority(ctx, request, guard, func(attempt context.Context, frozen DeliveryRequest, _ string) (bool, error) {
		retained, readErr := c.Store.GetCoordinationDelivery(attempt, frozen.Event, frozen.SinkID)
		if readErr != nil {
			return false, readErr
		}
		if retained != nil {
			if retained.Validate() != nil || retained.Event.Digest != frozen.Event.Digest || retained.SinkID != frozen.SinkID {
				return false, planning.ErrConflict
			}
			return true, nil
		}
		offered, _, freezeErr := frozen.Freeze()
		if freezeErr != nil {
			return false, freezeErr
		}
		if authorizeErr := c.Authority.AuthorizeCoordinationDelivery(attempt, offered); authorizeErr != nil {
			return false, authorizeErr
		}
		offered, _, freezeErr = frozen.Freeze()
		if freezeErr != nil {
			return false, freezeErr
		}
		receipt, deliverErr := c.Sink.DeliverCoordinationEvent(attempt, offered)
		if deliverErr != nil {
			return false, deliverErr
		}
		if receipt.Validate() != nil || receipt.Event.Digest != frozen.Event.Digest || receipt.SinkID != frozen.SinkID {
			return false, planning.ErrConflict
		}
		authorized, _, freezeErr := frozen.Freeze()
		if freezeErr != nil {
			return false, freezeErr
		}
		if authorizeErr := c.Authority.AuthorizeCoordinationDelivery(attempt, authorized); authorizeErr != nil {
			return false, authorizeErr
		}
		if contextErr := attempt.Err(); contextErr != nil {
			return false, contextErr
		}
		return true, c.Store.AcknowledgeCoordinationEvent(attempt, receipt)
	})
	return err
}

// Start begins authorized discovery and idempotent metadata delivery.
func (s *EventService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop signals cancellation without detaching owned sink calls.
func (s *EventService) RequestStop() { s.service.RequestStop() }

// Stop waits until all in-flight callbacks drain.
func (s *EventService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status reports process counters without private resource metadata.
func (s *EventService) Status() TaskServiceStatus { return s.service.Status() }
