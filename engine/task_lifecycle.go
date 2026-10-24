package engine

import (
	"context"
	"errors"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/internal/hostport"
)

// TaskSchedulingConfig installs host discovery and the engine's prepared task
// worker. Leave Service.Scheduler.Worker unset; Worker supplies guarded dispatch
// and work resolution. All other scheduler and lifecycle limits remain explicit.
type TaskSchedulingConfig struct {
	Service coordination.TaskServiceConfig
	Worker  TaskWorkerConfig
}

// WithTaskScheduling installs optional lifecycle scheduling before Start. The
// engine requires TaskRunAdmitter and TaskCallGuard after all options are applied,
// so option ordering does not change authorization requirements. Startup context
// values carry the host service identity; its cancellation is not the lifetime.
func WithTaskScheduling(config TaskSchedulingConfig) Option {
	return func(e *Engine) error {
		applied := config
		if e.taskScheduling != nil || !hostport.IsNil(applied.Service.Scheduler.Worker) || hostport.IsNil(applied.Worker.Resolver) || hostport.IsNil(applied.Worker.Dispatch) {
			return coordination.ErrInvalidScheduler
		}
		applied.Service.Scheduler.Worker = &taskWorker{engine: e, config: applied.Worker}
		report := applied.Service.ReportError
		applied.Service.ReportError = func(err error) {
			e.logError("scheduled task operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewTaskService(applied.Service)
		if err != nil {
			return err
		}
		e.taskScheduling, e.taskSchedulingWork = service, applied.Worker
		return nil
	}
}

func (e *Engine) configureTaskScheduling() error {
	if e.taskScheduling == nil {
		return nil
	}
	_, err := e.NewTaskWorker(e.taskSchedulingWork)
	return err
}

type taskLifecycleService interface {
	Start(context.Context) error
	RequestStop()
	Stop(context.Context) error
	Status() coordination.TaskServiceStatus
}

func (e *Engine) taskServices() []taskLifecycleService {
	var services []taskLifecycleService
	if e.taskScheduling != nil {
		services = append(services, e.taskScheduling)
	}
	if e.claimRecovery != nil {
		services = append(services, e.claimRecovery)
	}
	if e.providerRecovery != nil {
		services = append(services, e.providerRecovery)
	}
	if e.effectRecovery != nil {
		services = append(services, e.effectRecovery)
	}
	return services
}

func (e *Engine) stopTaskServices(ctx context.Context) error {
	services := e.taskServices()
	for _, service := range services {
		service.RequestStop()
	}
	var failures []error
	for _, service := range services {
		if err := service.Stop(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// TaskSchedulingStatus returns bounded lifecycle counters and whether scheduling
// was configured. A stopping service has not released its owned callbacks.
func (e *Engine) TaskSchedulingStatus() (coordination.TaskServiceStatus, bool) {
	if e.taskScheduling == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.taskScheduling.Status(), true
}
