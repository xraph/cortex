package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// ScheduledOrchestration identifies a team for maintenance, never for execution.
type ScheduledOrchestration struct {
	Scope cortex.Scope       `json:"scope"`
	ID    id.OrchestrationID `json:"id"`
}

// OrchestrationListRequest bounds host discovery using an opaque cursor.
type OrchestrationListRequest struct {
	After string
	Limit int
}

// OrchestrationPage contains at most the requested limit. An empty Next ends a
// pass. Sources must include closed accounts whose parent cleanup may be pending.
type OrchestrationPage struct {
	Teams []ScheduledOrchestration
	Next  string
}

// OrchestrationSource discovers team identities under the host service identity.
// The host authorizes discovery; maintenance restores each exact scope. A source
// must finish each pass and honor cancellation. It must never dispatch agents.
type OrchestrationSource interface {
	ListOrchestrations(context.Context, OrchestrationListRequest) (OrchestrationPage, error)
}

// OrchestrationExpirer closes expired admission and retains uncertain outcomes.
// Its implementation must check the stored deadline and cannot replay execution.
type OrchestrationExpirer interface {
	Expire(context.Context, id.OrchestrationID) error
}

// OrchestrationExpiryConfig installs bounded discovery and per-team maintenance.
// ReportError must return promptly. All callbacks must honor cancellation.
type OrchestrationExpiryConfig struct {
	Discovery                                  OrchestrationSource
	Expirer                                    OrchestrationExpirer
	Interval, DiscoveryTimeout, AttemptTimeout time.Duration
	MaxInFlight, MaxPerScope                   int
	ReportError                                func(error)
}

// OrchestrationExpiryService reuses the scheduler's bounded discovery, exact-scope
// fairness and joined shutdown. It cannot start children or settle reservations.
type OrchestrationExpiryService struct{ service *TaskService }

// NewOrchestrationExpiryService validates ports without starting work. Defaults
// are four concurrent teams, one per scope, one-second polling, five-second
// discovery and five-second expiry attempts. Interrupted work is rediscovered.
func NewOrchestrationExpiryService(config OrchestrationExpiryConfig) (*OrchestrationExpiryService, error) {
	if hostport.IsNil(config.Discovery) || hostport.IsNil(config.Expirer) {
		return nil, ErrInvalidScheduler
	}
	if config.MaxInFlight == 0 {
		config.MaxInFlight = 4
	}
	if config.MaxPerScope == 0 {
		config.MaxPerScope = 1
	}
	if config.AttemptTimeout == 0 {
		config.AttemptTimeout = 5 * time.Second
	}
	if config.MaxPerScope < 1 || config.MaxPerScope > 4096 || config.AttemptTimeout < time.Millisecond || config.AttemptTimeout > time.Minute {
		return nil, ErrInvalidScheduler
	}
	service, err := newPlanService(TaskServiceConfig{Discovery: orchestrationPlanSource{source: config.Discovery}, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, MaxInFlight: config.MaxInFlight, Scheduler: SchedulerConfig{MaxPerScope: config.MaxPerScope}, ReportError: config.ReportError}, func() (serviceScheduler, error) {
		return orchestrationExpiryScheduler{expirer: config.Expirer, timeout: config.AttemptTimeout}, nil
	})
	if err != nil {
		return nil, err
	}
	// Only this constructor accepts orchestration identities. Ordinary task and
	// recovery services retain their plan-prefix validation.
	service.identityPrefix = id.PrefixOrchestration
	return &OrchestrationExpiryService{service: service}, nil
}

// Start retains service identity values and owns an independent lifetime.
func (s *OrchestrationExpiryService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop signals all callbacks before drainage.
func (s *OrchestrationExpiryService) RequestStop() { s.service.RequestStop() }

// Stop joins discovery and expiry callbacks before returning success.
func (s *OrchestrationExpiryService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status returns process counters without team content or scope values.
func (s *OrchestrationExpiryService) Status() TaskServiceStatus { return s.service.Status() }

type orchestrationPlanSource struct{ source OrchestrationSource }

func (s orchestrationPlanSource) ListScheduledPlans(ctx context.Context, r TaskPlanListRequest) (TaskPlanPage, error) {
	page, err := s.source.ListOrchestrations(ctx, OrchestrationListRequest(r))
	if err != nil {
		return TaskPlanPage{}, err
	}
	if len(page.Teams) > r.Limit {
		return TaskPlanPage{}, ErrInvalidScheduler
	}
	out := TaskPlanPage{Next: page.Next, Plans: make([]ScheduledPlan, 0, len(page.Teams))}
	for _, team := range page.Teams {
		out.Plans = append(out.Plans, ScheduledPlan{Scope: team.Scope, PlanID: team.ID})
	}
	return out, nil
}

type orchestrationExpiryScheduler struct {
	expirer OrchestrationExpirer
	timeout time.Duration
}

func (s orchestrationExpiryScheduler) Tick(ctx context.Context, team id.ID) error {
	attempt, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.expirer.Expire(attempt, team); err != nil {
		return err
	}
	return attempt.Err()
}
func (orchestrationExpiryScheduler) Shutdown(context.Context) error { return nil }
