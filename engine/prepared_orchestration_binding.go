package engine

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

func hasOrchestrationBinding(p run.ExecutionProvenance) bool {
	return p.OrchestrationID != "" || p.OrchestrationDigest != "" || p.OrchestrationReservationID != ""
}

func validOrchestrationBinding(p run.ExecutionProvenance) bool {
	team, teamErr := id.Parse(p.OrchestrationID)
	reservation, reservationErr := id.Parse(p.OrchestrationReservationID)
	return teamErr == nil && !team.IsNil() && team.Prefix() == id.PrefixOrchestration && reservationErr == nil && !reservation.IsNil() && reservation.Prefix() == execution.PrefixReservation && evidence.ValidDigest(p.OrchestrationDigest)
}

func (e *Engine) checkOrchestrationBinding(ctx context.Context, key id.AgentRunID, snap *preparedSnapshot, p run.ExecutionProvenance, budget execution.Budget) error {
	return e.checkOrchestrationAuthority(ctx, key, snap, p, budget, false)
}

// Completed observations may read a settled reservation, but execution and
// paused-child resume still require an unsettled reservation.
func (e *Engine) checkOrchestrationObservation(ctx context.Context, child *run.Run, snap *preparedSnapshot) error {
	return e.checkOrchestrationAuthority(ctx, child.ID, snap, child.Provenance, child.Budget, child.State == run.StateCompleted)
}

func (e *Engine) checkOrchestrationAuthority(ctx context.Context, key id.AgentRunID, snap *preparedSnapshot, p run.ExecutionProvenance, budget execution.Budget, completedObservation bool) error {
	if err := orchestration.CheckCoordinator(ctx); err != nil {
		return err
	}
	if !hasOrchestrationBinding(p) {
		return nil
	}
	if !validOrchestrationBinding(p) || snap == nil {
		return cortex.ErrInvalidPrepared
	}
	storage, ok := e.store.(orchestration.BudgetStore)
	if !ok {
		return cortex.ErrBudgetUnavailable
	}
	team, err := id.Parse(p.OrchestrationID)
	if err != nil {
		return cortex.ErrInvalidPrepared
	}
	reservationID, err := id.Parse(p.OrchestrationReservationID)
	if err != nil {
		return cortex.ErrInvalidPrepared
	}
	account, err := storage.GetOrchestrationBudgetSnapshot(ctx, team)
	if err != nil {
		return err
	}
	if account.Closed || account.Overflow || !account.Policy.Limits.Covers(account.Charged) || account.Policy.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() || account.Policy.PreparedDigest != p.OrchestrationDigest || account.Policy.ParticipantDigests[snap.Agent.Name] != p.PreparedDigest || account.Policy.Scope.Canonical() != snap.Scope.Canonical() {
		return cortex.ErrInvalidPrepared
	}
	teamRun, teamErr := e.store.GetOrchestrationRun(ctx, team)
	if teamErr != nil {
		return errors.Join(cortex.ErrInvalidPrepared, teamErr)
	}
	if (teamRun.Status != orchestration.StatusRunning && teamRun.Status != orchestration.StatusWaiting) || teamRun.Scope.Canonical() != account.Policy.Scope.Canonical() {
		return cortex.ErrInvalidPrepared
	}
	reservation, err := storage.GetReservation(ctx, reservationID)
	if err != nil {
		return err
	}
	req := reservation.Request
	allocation, err := preparedTeamAllocation(snap)
	if err != nil {
		return err
	}
	if account.Policy.ValidateReservation(req) != nil || (reservation.Settlement != nil && !completedObservation) || req.TaskID != key || req.OwnerRef != snap.Agent.Name || !req.Allocation.Covers(allocation) || budget.Deadline.IsZero() || budget.Deadline.After(req.Deadline) || !time.Now().Before(req.Deadline) {
		return cortex.ErrInvalidPrepared
	}
	return nil
}

func (e *Engine) checkPreparedTeamResume(ctx context.Context, r *run.Run, s *suspension.Suspension) error {
	if !hasOrchestrationBinding(r.Provenance) {
		return nil
	}
	if s == nil {
		return cortex.ErrInvalidContinuation
	}
	_, snap, err := decodeContinuation(s.Cont)
	if err != nil {
		return err
	}
	if validationErr := validateFrozenContinuation(s.Cont, snap, r); validationErr != nil {
		return validationErr
	}
	return e.checkOrchestrationBinding(ctx, r.ID, snap, r.Provenance, r.Budget)
}
