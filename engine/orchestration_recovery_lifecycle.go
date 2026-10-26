package engine

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
)

// OrchestrationRecoveryConfig installs the engine's guarded continuation runner.
// Leave Service.Recoverer unset. Authorize must check the current host service
// identity for this exact team before recovery and each provider or tool call.
type OrchestrationRecoveryConfig struct {
	Service   coordination.OrchestrationRecoveryConfig
	Authorize func(context.Context, id.OrchestrationID) error
}

// WithOrchestrationRecovery enables explicit bounded background recovery before
// Start. Waiting teams are revisited when children resolve; expired running
// owners are reclaimed through the same guarded API as explicit recovery.
func WithOrchestrationRecovery(config OrchestrationRecoveryConfig) Option {
	return func(e *Engine) error {
		if e.orchestrationRecovery != nil || config.Authorize == nil || !hostport.IsNil(config.Service.Recoverer) {
			return coordination.ErrInvalidScheduler
		}
		applied := config.Service
		applied.Recoverer = &preparedTeamRecovery{engine: e, authorize: config.Authorize}
		report := applied.ReportError
		applied.ReportError = func(err error) {
			e.logError("orchestration recovery operation failed", err)
			if report != nil {
				report(err)
			}
		}
		service, err := coordination.NewOrchestrationRecoveryService(applied)
		if err != nil {
			return err
		}
		e.orchestrationRecovery = service
		return nil
	}
}

// OrchestrationRecoveryStatus reports configuration and local lifecycle counters.
func (e *Engine) OrchestrationRecoveryStatus() (coordination.TaskServiceStatus, bool) {
	if e.orchestrationRecovery == nil {
		return coordination.TaskServiceStatus{}, false
	}
	return e.orchestrationRecovery.Status(), true
}

func (e *Engine) configureOrchestrationRecovery() error {
	if e.orchestrationRecovery == nil {
		return nil
	}
	if e.store == nil {
		return cortex.ErrNoStore
	}
	if e.llm == nil {
		return cortex.ErrNoLLM
	}
	if _, ok := e.store.(orchestration.CoordinatorStore); !ok {
		return cortex.ErrOrchestrationContinuationUnavailable
	}
	if _, ok := e.store.(orchestration.ContinuationStore); !ok {
		return cortex.ErrOrchestrationContinuationUnavailable
	}
	if _, ok := e.store.(orchestration.ContinuationRunStore); !ok {
		return cortex.ErrOrchestrationContinuationUnavailable
	}
	if _, ok := e.store.(orchestration.BudgetStore); !ok {
		return cortex.ErrBudgetUnavailable
	}
	return nil
}

type teamRecoveryAuthorityKey struct{}
type teamRecoveryAuthority struct {
	team      id.OrchestrationID
	authorize func(context.Context, id.OrchestrationID) error
}

func checkTeamRecoveryAuthority(ctx context.Context, team id.OrchestrationID) error {
	authority, ok := ctx.Value(teamRecoveryAuthorityKey{}).(teamRecoveryAuthority)
	if !ok {
		return nil
	}
	if authority.team != team || authority.authorize == nil {
		return cortex.ErrInvalidPrepared
	}
	return authority.authorize(ctx, team)
}

type preparedTeamRecovery struct {
	engine    *Engine
	authorize func(context.Context, id.OrchestrationID) error
}

func (r *preparedTeamRecovery) Recover(ctx context.Context, team id.OrchestrationID) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	if team.IsNil() || team.Prefix() != id.PrefixOrchestration {
		return cortex.ErrInvalidContinuation
	}
	if authErr := r.authorize(ctx, team); authErr != nil {
		return authErr
	}
	parent, err := r.engine.store.GetOrchestrationRun(ctx, team)
	if errors.Is(err, cortex.ErrOrchestrationRunNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if parent == nil || parent.ID != team || parent.Scope.Canonical() != scope.Canonical() {
		return cortex.ErrInvalidContinuation
	}
	if parent.ContinuationGeneration == 0 || parent.Status == orchestration.StatusCompleted || parent.Status == orchestration.StatusFailed {
		return nil
	}
	budgets, ok := r.engine.store.(orchestration.BudgetStore)
	if !ok {
		return cortex.ErrBudgetUnavailable
	}
	account, err := budgets.GetOrchestrationBudgetSnapshot(ctx, team)
	if err != nil {
		return err
	}
	if account.Policy.ExecutionID != team || account.Policy.Scope.Canonical() != scope.Canonical() {
		return cortex.ErrInvalidPrepared
	}
	if account.Closed || !time.Now().Before(account.Policy.Deadline) {
		return nil
	}
	switch parent.Status {
	case orchestration.StatusRunning:
		owners, available := r.engine.store.(orchestration.CoordinatorStore)
		if !available {
			return cortex.ErrOrchestrationContinuationUnavailable
		}
		lease, readErr := owners.GetOrchestrationCoordinator(ctx, team)
		if errors.Is(readErr, cortex.ErrOrchestrationCoordinatorNotFound) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
		if lease.Released || lease.TeamID != team || lease.Scope.Canonical() != scope.Canonical() || lease.Generation != parent.ContinuationGeneration {
			return cortex.ErrInvalidContinuation
		}
		if time.Now().Before(lease.ExpiresAt) {
			return nil
		}
	case orchestration.StatusWaiting:
		ready, readErr := r.waitingReady(ctx, parent)
		if readErr != nil || !ready {
			return readErr
		}
	default:
		return cortex.ErrInvalidContinuation
	}
	ctx = context.WithValue(ctx, teamRecoveryAuthorityKey{}, teamRecoveryAuthority{team: team, authorize: r.authorize})
	if authErr := checkTeamRecoveryAuthority(ctx, team); authErr != nil {
		return authErr
	}
	result, resumeErr := r.engine.ResumePreparedOrchestration(ctx, team)
	if result == nil && errors.Is(resumeErr, cortex.ErrOrchestrationRunConflict) {
		return nil
	}
	if orchestration.IsWaiting(resumeErr) {
		return nil
	}
	return resumeErr
}

func (r *preparedTeamRecovery) waitingReady(ctx context.Context, parent *orchestration.Run) (bool, error) {
	// Bound each observation pass independently of the immutable team fan-out.
	if len(parent.AgentRunIDs) == 0 || len(parent.AgentRunIDs) > 4096 {
		return false, cortex.ErrInvalidContinuation
	}
	waiting, failed := false, false
	for _, key := range parent.AgentRunIDs {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		child, err := r.engine.store.GetRun(ctx, key)
		if err != nil {
			return false, err
		}
		if child == nil || child.ID != key || child.Scope.Canonical() != parent.Scope.Canonical() || child.Provenance.OrchestrationID != parent.ID.String() {
			return false, cortex.ErrInvalidPrepared
		}
		switch child.State {
		case run.StatePaused, run.StateRunning:
			waiting = true
		case run.StateCompleted:
			if verificationErr := requiredChildResultError(child); errors.Is(verificationErr, cortex.ErrOrchestrationWaiting) {
				waiting = true
			} else if verificationErr != nil {
				failed = true
			}
		case run.StateCreated, run.StateIncomplete, run.StateCancelled, run.StateFailed:
			failed = true
		default:
			return false, cortex.ErrInvalidContinuation
		}
	}
	if waiting && !failed {
		return r.engine.preparedMessageWorkReady(ctx, parent)
	}
	return failed || !waiting, nil
}
