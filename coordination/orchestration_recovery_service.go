package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// OrchestrationRecoverer revalidates current host authority and durable ownership
// before continuing a team. Discovery alone never grants execution permission.
type OrchestrationRecoverer interface {
	Recover(context.Context, id.OrchestrationID) error
}

// OrchestrationRecoveryConfig bounds discovery and synchronous recovery passes.
// A timed-out attempt cancels its execution; the runner must retain uncertainty.
// All callbacks must honor cancellation, and ReportError must return promptly.
type OrchestrationRecoveryConfig struct {
	Discovery                                  OrchestrationSource
	Recoverer                                  OrchestrationRecoverer
	Interval, DiscoveryTimeout, AttemptTimeout time.Duration
	MaxInFlight, MaxPerScope                   int
	ReportError                                func(error)
}

// OrchestrationRecoveryService owns bounded recovery passes and joined shutdown.
type OrchestrationRecoveryService struct{ service *TaskService }

// NewOrchestrationRecoveryService defaults to four concurrent teams, one per
// scope, one-second polling, five-second discovery and a one-minute attempt. Attempts are capped at
// thirty minutes. It starts no work until Start is called.
func NewOrchestrationRecoveryService(config OrchestrationRecoveryConfig) (*OrchestrationRecoveryService, error) {
	if hostport.IsNil(config.Discovery) || hostport.IsNil(config.Recoverer) {
		return nil, ErrInvalidScheduler
	}
	if config.AttemptTimeout == 0 {
		config.AttemptTimeout = time.Minute
	}
	if config.AttemptTimeout < time.Millisecond || config.AttemptTimeout > 30*time.Minute {
		return nil, ErrInvalidScheduler
	}
	service, err := newIdentityOperation(id.PrefixOrchestration, TaskServiceConfig{Discovery: orchestrationPlanSource{source: config.Discovery}, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, MaxInFlight: config.MaxInFlight, Scheduler: SchedulerConfig{MaxPerScope: config.MaxPerScope}, ReportError: config.ReportError}, config.AttemptTimeout, config.Recoverer.Recover)
	if err != nil {
		return nil, err
	}
	return &OrchestrationRecoveryService{service: service}, nil
}

// Start retains startup identity while owning an independent service lifetime.
func (s *OrchestrationRecoveryService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop cancels discovery and every owned attempt before drainage.
func (s *OrchestrationRecoveryService) RequestStop() { s.service.RequestStop() }

// Stop succeeds only after discovery and all recovery callbacks drain.
func (s *OrchestrationRecoveryService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status returns local counters without team content or scope values.
func (s *OrchestrationRecoveryService) Status() TaskServiceStatus { return s.service.Status() }
