package orchestration

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// HookEmitter receives orchestration lifecycle events. The engine adapts the
// plugin.Registry to this interface; tests pass a recorder.
type HookEmitter interface {
	OrchestrationStarted(ctx context.Context, orchID id.OrchestrationID, strategy string)
	OrchestrationCompleted(ctx context.Context, orchID id.OrchestrationID, elapsed time.Duration)
	AgentHandoff(ctx context.Context, orchID id.OrchestrationID, from, to, payload string)
}

// noopHooks is used when no emitter is supplied.
type noopHooks struct{}

func (noopHooks) OrchestrationStarted(context.Context, id.OrchestrationID, string)          {}
func (noopHooks) OrchestrationCompleted(context.Context, id.OrchestrationID, time.Duration) {}
func (noopHooks) AgentHandoff(context.Context, id.OrchestrationID, string, string, string)  {}

// Service runs stored orchestration configs end to end.
type Service struct {
	runner  AgentRunner
	configs ConfigStore
	runs    RunStore
	hooks   HookEmitter
}

// NewService builds a Service. hooks may be nil (a no-op emitter is used).
func NewService(runner AgentRunner, configs ConfigStore, runs RunStore, hooks HookEmitter) *Service {
	if hooks == nil {
		hooks = noopHooks{}
	}
	return &Service{runner: runner, configs: configs, runs: runs, hooks: hooks}
}

// Run loads the named config, executes the strategy, persists a Run,
// and fires lifecycle hooks. The returned run reflects the final state.
func (s *Service) Run(ctx context.Context, appID, name, input string) (*Run, error) {
	cfg, err := s.configs.GetOrchestrationByName(ctx, appID, name)
	if err != nil {
		return nil, err
	}

	orch, err := Build(cfg.Strategy, s.runner, cfg.AppID, cfg.Participants, cfg.Settings)
	if err != nil {
		return nil, err
	}

	started := nowUTC()
	rec := &Run{
		ID:        id.NewOrchestrationID(),
		ConfigID:  cfg.ID,
		AppID:     cfg.AppID,
		TenantID:  cortexTenant(ctx),
		Strategy:  cfg.Strategy,
		Status:    StatusRunning,
		Input:     input,
		StartedAt: started,
	}
	if err := s.runs.CreateOrchestrationRun(ctx, rec); err != nil {
		return nil, err
	}
	orchID := rec.ID
	s.hooks.OrchestrationStarted(ctx, orchID, cfg.Strategy)

	bb := NewBlackboard(orchID, cfg.Participants, func(hctx context.Context, h Handoff) {
		s.hooks.AgentHandoff(hctx, orchID, h.From, h.To, h.Payload)
	})

	result, runErr := orch.Run(ctx, input, bb)

	completed := nowUTC()
	rec.CompletedAt = &completed
	if result != nil {
		rec.Output = result.Output
		for _, ar := range result.AgentResults {
			if !ar.RunID.IsNil() {
				rec.AgentRunIDs = append(rec.AgentRunIDs, ar.RunID)
			}
		}
	}
	if runErr != nil {
		rec.Status = StatusFailed
		rec.Error = runErr.Error()
	} else {
		rec.Status = StatusCompleted
	}
	if err := s.runs.UpdateOrchestrationRun(ctx, rec); err != nil {
		return nil, err
	}
	s.hooks.OrchestrationCompleted(ctx, orchID, completed.Sub(started))

	return rec, runErr
}
