package coordination

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
)

// ScheduledAgentReply identifies a run to inspect, without granting execution.
type ScheduledAgentReply struct {
	Scope cortex.Scope  `json:"scope"`
	ID    id.AgentRunID `json:"id"`
}

// AgentReplyListRequest bounds host discovery with an opaque cursor.
type AgentReplyListRequest struct {
	After string
	Limit int
}

// AgentReplyPage contains at most the requested limit of distinct scoped runs.
// Empty Next ends a pass; a later pass revisits skipped and interrupted work.
type AgentReplyPage struct {
	Runs []ScheduledAgentReply
	Next string
}

// AgentReplySource discovers paused or admitted runs under current host authority.
// Include runs with retained answers even if admission has not been recorded.
// Complete each pass, honor cancellation and never execute agents in this port.
type AgentReplySource interface {
	ListAgentReplyRuns(context.Context, AgentReplyListRequest) (AgentReplyPage, error)
}

// AgentReplyRecoverer validates the current suspension and authority before resume.
type AgentReplyRecoverer interface {
	Recover(context.Context, id.AgentRunID) error
}

// AgentReplyRecoveryConfig bounds discovery and synchronous recovery attempts.
// Callbacks must honor cancellation. ReportError must return promptly.
type AgentReplyRecoveryConfig struct {
	Discovery                                  AgentReplySource
	Recoverer                                  AgentReplyRecoverer
	Interval, DiscoveryTimeout, AttemptTimeout time.Duration
	MaxInFlight, MaxPerScope                   int
	ReportError                                func(error)
}

// AgentReplyRecoveryService owns discovery, per-scope fairness and joined shutdown.
type AgentReplyRecoveryService struct{ service *TaskService }

// NewAgentReplyRecoveryService defaults to four concurrent runs, one per scope,
// one-second polling, five-second discovery and one-minute attempts. Attempts
// cannot exceed thirty minutes. Construction starts no background work.
func NewAgentReplyRecoveryService(config AgentReplyRecoveryConfig) (*AgentReplyRecoveryService, error) {
	if hostport.IsNil(config.Discovery) || hostport.IsNil(config.Recoverer) {
		return nil, ErrInvalidScheduler
	}
	if config.AttemptTimeout == 0 {
		config.AttemptTimeout = time.Minute
	}
	if config.AttemptTimeout < time.Millisecond || config.AttemptTimeout > 30*time.Minute {
		return nil, ErrInvalidScheduler
	}
	service, err := newIdentityOperation(id.PrefixAgentRun, TaskServiceConfig{Discovery: agentReplyPlanSource{source: config.Discovery}, Interval: config.Interval, DiscoveryTimeout: config.DiscoveryTimeout, MaxInFlight: config.MaxInFlight, Scheduler: SchedulerConfig{MaxPerScope: config.MaxPerScope}, ReportError: config.ReportError}, config.AttemptTimeout, config.Recoverer.Recover)
	if err != nil {
		return nil, err
	}
	return &AgentReplyRecoveryService{service: service}, nil
}

// Start preserves host identity while owning an independent service lifetime.
func (s *AgentReplyRecoveryService) Start(ctx context.Context) error { return s.service.Start(ctx) }

// RequestStop cancels discovery and every owned attempt before drainage.
func (s *AgentReplyRecoveryService) RequestStop() { s.service.RequestStop() }

// Stop succeeds only after discovery and all recovery callbacks drain.
func (s *AgentReplyRecoveryService) Stop(ctx context.Context) error { return s.service.Stop(ctx) }

// Status returns local counters without run content or scope values.
func (s *AgentReplyRecoveryService) Status() TaskServiceStatus { return s.service.Status() }

type agentReplyPlanSource struct{ source AgentReplySource }

func (s agentReplyPlanSource) ListScheduledPlans(ctx context.Context, req TaskPlanListRequest) (TaskPlanPage, error) {
	page, err := s.source.ListAgentReplyRuns(ctx, AgentReplyListRequest(req))
	if err != nil {
		return TaskPlanPage{}, err
	}
	if len(page.Runs) > req.Limit {
		return TaskPlanPage{}, ErrInvalidScheduler
	}
	result := TaskPlanPage{Next: page.Next, Plans: make([]ScheduledPlan, 0, len(page.Runs))}
	for _, candidate := range page.Runs {
		result.Plans = append(result.Plans, ScheduledPlan{Scope: candidate.Scope, PlanID: candidate.ID})
	}
	return result, nil
}
