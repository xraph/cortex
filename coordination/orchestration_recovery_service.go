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
	service, err := newOrchestrationOperation(TaskServiceConfig{Discovery: orchestrationPlanSource{source: config.Discovery}, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, MaxInFlight: config.MaxInFlight, Scheduler: SchedulerConfig{MaxPerScope: config.MaxPerScope}, ReportError: config.ReportError}, config.AttemptTimeout, config.Recoverer.Recover)
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

func newOrchestrationOperation(config TaskServiceConfig, timeout time.Duration, operation func(context.Context, id.OrchestrationID) error) (*TaskService, error) {
	if config.MaxInFlight == 0 {
		config.MaxInFlight = 4
	}
	if config.Scheduler.MaxPerScope == 0 {
		config.Scheduler.MaxPerScope = 1
	}
	if config.Scheduler.MaxPerScope < 1 || config.Scheduler.MaxPerScope > 4096 {
		return nil, ErrInvalidScheduler
	}
	service, err := newPlanService(config, func() (serviceScheduler, error) {
		return orchestrationOperationScheduler{operation: operation, timeout: timeout}, nil
	})
	if err != nil {
		return nil, err
	}
	service.identityPrefix = id.PrefixOrchestration
	return service, nil
}

type orchestrationOperationScheduler struct {
	operation func(context.Context, id.OrchestrationID) error
	timeout   time.Duration
}

func (s orchestrationOperationScheduler) Tick(ctx context.Context, team id.ID) error {
	attempt, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.operation(attempt, team); err != nil {
		return err
	}
	return attempt.Err()
}
func (orchestrationOperationScheduler) Shutdown(context.Context) error { return nil }
