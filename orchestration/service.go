package orchestration

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/xraph/cortex"
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
func (s *Service) Run(ctx context.Context, name, input string) (*Run, error) {
	cfg, err := s.configs.GetOrchestrationByName(ctx, name)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, cortex.ErrOrchestrationNotFound
	}
	return s.RunConfig(ctx, *cfg, input)
}

// RunConfig executes the supplied config without reading ConfigStore. Execution
// fields are detached before hooks or agents run. The injected runner remains
// responsible for participant preparation, authorization and shared budgets.
// Child identities are persisted as each invocation returns. A failed write
// stops subsequent admission; already admitted parallel calls retain their IDs.
func (s *Service) RunConfig(ctx context.Context, cfg Config, input string) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cfg.Participants = slices.Clone(cfg.Participants)
	for i := range cfg.Participants {
		cfg.Participants[i].Skills = slices.Clone(cfg.Participants[i].Skills)
	}
	cfg.Settings.RouterRules = maps.Clone(cfg.Settings.RouterRules)
	rec := &Run{
		ID: id.NewOrchestrationID(), ConfigID: cfg.ID, Strategy: cfg.Strategy,
		Status: StatusRunning, Input: input, StartedAt: nowUTC(),
	}
	runner := &persistingRunner{runner: s.runner, runs: s.runs, record: rec}

	orch, err := Build(cfg.Strategy, runner, cfg.Participants, cfg.Settings)
	if err != nil {
		return nil, err
	}

	started := rec.StartedAt
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
		observed := rec.AgentRunIDs
		rec.AgentRunIDs = nil
		for _, ar := range result.AgentResults {
			if !ar.RunID.IsNil() {
				rec.AgentRunIDs = append(rec.AgentRunIDs, ar.RunID)
			}
		}
		// Keep strategy ordering while retaining any observed child the result
		// omitted. Partial evidence must not depend on successful synthesis.
		for _, child := range observed {
			if !slices.Contains(rec.AgentRunIDs, child) {
				rec.AgentRunIDs = append(rec.AgentRunIDs, child)
			}
		}
	}
	if runErr != nil {
		rec.Status = StatusFailed
		rec.Error = runErr.Error()
	} else {
		rec.Status = StatusCompleted
	}
	if err := persistOrchestrationRun(ctx, s.runs, rec); err != nil {
		return rec, errors.Join(runErr, err)
	}
	s.hooks.OrchestrationCompleted(ctx, orchID, completed.Sub(started))

	return rec, runErr
}

func persistOrchestrationRun(ctx context.Context, runs RunStore, rec *Run) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return runs.UpdateOrchestrationRun(writeCtx, rec)
}
