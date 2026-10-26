package orchestration

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
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
	coordinated bool
	observer    ChildObserver
	runner      AgentRunner
	reserved    ReservedAgentRunner
	configs     ConfigStore
	runs        RunStore
	hooks       HookEmitter
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
	if hostport.IsNil(s.configs) {
		return nil, cortex.ErrOrchestrationNotFound
	}
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
// Reserved runners persist child identities before execution; ordinary runners
// persist them when they return. A failed write stops subsequent admission.
func (s *Service) RunConfig(ctx context.Context, cfg Config, input string) (*Run, error) {
	return s.RunConfigWithID(ctx, cfg, input, id.NewOrchestrationID())
}

func (s *Service) runConfig(ctx context.Context, cfg Config, input string, team id.OrchestrationID) (*Run, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if s.observer != nil {
		storage, ok := s.runs.(ContinuationStore)
		if !ok {
			return nil, cortex.ErrOrchestrationContinuationUnavailable
		}
		captured, readErr := storage.GetOrchestrationContinuation(ctx, team)
		if readErr != nil {
			return nil, readErr
		}
		desired := captured
		desired.Config = cfg
		desired.Input = input
		_, want, freezeErr := desired.Freeze()
		_, got, captureErr := captured.Freeze()
		if freezeErr != nil || captureErr != nil || want != got {
			return nil, cortex.ErrInvalidContinuation
		}
	}
	cfg.Participants = slices.Clone(cfg.Participants)
	for i := range cfg.Participants {
		cfg.Participants[i].Skills = slices.Clone(cfg.Participants[i].Skills)
	}
	cfg.Settings.RouterRules = maps.Clone(cfg.Settings.RouterRules)
	rec := &Run{
		ID: team, ConfigID: cfg.ID, Strategy: cfg.Strategy,
		Status: StatusRunning, Input: input, StartedAt: nowUTC(),
	}
	if s.observer != nil {
		rec.ContinuationGeneration = 1
	}
	if _, err := Build(cfg.Strategy, nil, cfg.Participants, cfg.Settings); err != nil {
		return nil, err
	}
	if err := s.runs.CreateOrchestrationRun(ctx, rec); err != nil {
		return nil, err
	}
	if s.coordinated {
		ownedCtx, pass, claimed, claimErr := s.acquireCoordinator(ctx, rec, true)
		if claimErr != nil {
			return nil, claimErr
		}
		// Install cleanup before host hooks, which may panic.
		defer pass.stop()
		return s.executeCoordinated(ownedCtx, cfg, input, claimed, false)
	}
	s.hooks.OrchestrationStarted(ctx, rec.ID, cfg.Strategy)
	return s.executeConfig(ctx, cfg, input, rec, false)
}

func (s *Service) executeConfig(ctx context.Context, cfg Config, input string, rec *Run, replay bool) (*Run, error) {
	runner := &persistingRunner{runner: s.runner, reserved: s.reserved, runs: s.runs, record: rec, observer: s.observer}
	orch, err := Build(cfg.Strategy, runner, cfg.Participants, cfg.Settings)
	if err != nil {
		return rec, err
	}
	started, orchID := rec.StartedAt, rec.ID
	bb := NewBlackboard(orchID, cfg.Participants, func(hctx context.Context, h Handoff) {
		if !replay {
			s.hooks.AgentHandoff(hctx, orchID, h.From, h.To, h.Payload)
		}
	})

	var work CoordinatorWork
	if s.coordinated {
		if candidate, supported := s.reserved.(CoordinatorWork); supported {
			work = candidate
		}
	}
	var runErr error
	if work != nil {
		runErr = work.ReconcileOrchestration(ctx)
		if IsWaiting(runErr) {
			runErr = nil
		}
	}
	var result *Result
	if runErr == nil {
		result, runErr = orch.Run(ctx, input, bb)
	}
	if work != nil && (runErr == nil || IsWaiting(runErr)) {
		runErr = errors.Join(runErr, work.ReconcileOrchestration(ctx))
	}
	if ctx.Err() != nil {
		runErr = errors.Join(runErr, ctx.Err())
	}

	completed := nowUTC()
	rec.CompletedAt = &completed
	if result != nil {
		rec.Output = result.Output
		observed := rec.AgentRunIDs
		rec.AgentRunIDs = nil
		for _, ar := range result.AgentResults {
			if !ar.RunID.IsNil() && !slices.Contains(rec.AgentRunIDs, ar.RunID) {
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
	switch {
	case s.observer != nil && IsWaiting(runErr):
		rec.Status = StatusWaiting
		rec.CompletedAt = nil
		rec.Error = ""
		rec.Output = ""
	case runErr != nil:
		rec.Status = StatusFailed
		rec.Error = runErr.Error()
	default:
		rec.Status = StatusCompleted
	}
	if err := persistOrchestrationRun(ctx, s.runs, rec); err != nil {
		return rec, errors.Join(runErr, err)
	}
	if rec.Status != StatusWaiting {
		s.hooks.OrchestrationCompleted(ctx, orchID, completed.Sub(started))
	}

	return rec, runErr
}

func persistOrchestrationRun(ctx context.Context, runs RunStore, rec *Run) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if pass := coordinatorFromContext(ctx); pass != nil {
		return pass.finish(writeCtx, rec)
	}
	return runs.UpdateOrchestrationRun(writeCtx, rec)
}
