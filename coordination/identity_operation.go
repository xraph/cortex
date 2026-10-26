package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

func newIdentityOperation(prefix id.Prefix, config TaskServiceConfig, timeout time.Duration, operation func(context.Context, id.ID) error) (*TaskService, error) {
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
		return identityOperationScheduler{operation: operation, timeout: timeout}, nil
	})
	if err != nil {
		return nil, err
	}
	service.identityPrefix = prefix
	return service, nil
}

type identityOperationScheduler struct {
	operation func(context.Context, id.ID) error
	timeout   time.Duration
}

func (s identityOperationScheduler) Tick(ctx context.Context, key id.ID) error {
	attempt, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.operation(attempt, key); err != nil {
		return err
	}
	return attempt.Err()
}
func (identityOperationScheduler) Shutdown(context.Context) error { return nil }
