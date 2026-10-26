package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/verification"
)

// ScheduledVerificationJob identifies captured work without granting execution.
type ScheduledVerificationJob struct {
	Scope cortex.Scope `json:"scope"`
	ID    id.ID        `json:"id"`
}

// VerificationJobListRequest bounds discovery with an opaque host cursor.
type VerificationJobListRequest struct {
	After string
	Limit int
}

// VerificationJobPage contains scoped identities. Empty Next ends a pass.
type VerificationJobPage struct {
	Jobs []ScheduledVerificationJob
	Next string
}

// VerificationJobSource discovers captured jobs under current host authority.
// It must finish each pass and must not evaluate or generate candidate content.
type VerificationJobSource interface {
	ListCapturedVerificationJobs(context.Context, VerificationJobListRequest) (VerificationJobPage, error)
}

// VerificationJobRecoverer rechecks saved state and current authority per job.
type VerificationJobRecoverer interface {
	Recover(context.Context, id.ID) error
}

// VerificationRecoveryConfig bounds discovery and synchronous recovery attempts.
type VerificationRecoveryConfig struct {
	Discovery                                  VerificationJobSource
	Recoverer                                  VerificationJobRecoverer
	Interval, DiscoveryTimeout, AttemptTimeout time.Duration
	MaxInFlight, MaxPerScope                   int
	ReportError                                func(error)
}

// VerificationRecoveryService owns bounded discovery, fairness and joined shutdown.
type VerificationRecoveryService struct{ service *TaskService }

// NewVerificationRecoveryService defaults to four concurrent jobs, one per scope,
// one-second polling and one-minute attempts. Construction starts no background work.
func NewVerificationRecoveryService(config VerificationRecoveryConfig) (*VerificationRecoveryService, error) {
	if hostport.IsNil(config.Discovery) || hostport.IsNil(config.Recoverer) {
		return nil, ErrInvalidScheduler
	}
	if config.AttemptTimeout == 0 {
		config.AttemptTimeout = time.Minute
	}
	if config.AttemptTimeout < time.Millisecond || config.AttemptTimeout > 30*time.Minute {
		return nil, ErrInvalidScheduler
	}
	service, err := newIdentityOperation(verification.PrefixJob, TaskServiceConfig{Discovery: verificationPlanSource{config.Discovery}, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, MaxInFlight: config.MaxInFlight, Scheduler: SchedulerConfig{MaxPerScope: config.MaxPerScope}, ReportError: config.ReportError}, config.AttemptTimeout, config.Recoverer.Recover)
	if err != nil {
		return nil, err
	}
	return &VerificationRecoveryService{service: service}, nil
}

// Start preserves trusted host context while owning its own service lifetime.
func (s *VerificationRecoveryService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop cancels discovery and all owned recovery attempts.
func (s *VerificationRecoveryService) RequestStop() { s.service.RequestStop() }

// Stop joins discovery and attempts before storage can be disposed.
func (s *VerificationRecoveryService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status returns local counters without scope or candidate content.
func (s *VerificationRecoveryService) Status() TaskServiceStatus { return s.service.Status() }

type verificationPlanSource struct{ source VerificationJobSource }

func (s verificationPlanSource) ListScheduledPlans(ctx context.Context, req TaskPlanListRequest) (TaskPlanPage, error) {
	page, err := s.source.ListCapturedVerificationJobs(ctx, VerificationJobListRequest(req))
	if err != nil {
		return TaskPlanPage{}, err
	}
	if len(page.Jobs) > req.Limit {
		return TaskPlanPage{}, ErrInvalidScheduler
	}
	result := TaskPlanPage{Next: page.Next, Plans: make([]ScheduledPlan, 0, len(page.Jobs))}
	for _, job := range page.Jobs {
		result.Plans = append(result.Plans, ScheduledPlan{Scope: job.Scope, PlanID: job.ID})
	}
	return result, nil
}
