package engine

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/internal/telemetry"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
)

// PlanReplanInputSource resolves the exact prepared proposal configuration and
// input retained by host identity. It must honor cancellation. The adapter checks
// both hashes, the agent, explicit tool denial and reserved runtime ceilings.
type PlanReplanInputSource interface {
	ResolvePlanReplanInput(context.Context, coordination.PlanReplanRequest) (*PreparedAgent, string, error)
}

type preparedPlanReplanRuntime struct {
	engine *Engine
	source PlanReplanInputSource
}

// NewPlanReplanRuntime adapts admitted proposals to the engine's normal prepared
// ReAct loop. Use it only inside PlanReplanner's current host execution fence.
// Existing or ambiguous RunID creation is never retried as model dispatch.
func (e *Engine) NewPlanReplanRuntime(source PlanReplanInputSource) (coordination.PlanReplanRuntime, error) {
	if e.store == nil || hostport.IsNil(source) {
		return nil, planning.ErrInvalid
	}
	if _, ok := e.store.(coordination.PlanReplanStore); !ok {
		return nil, planning.ErrInvalid
	}
	return &preparedPlanReplanRuntime{engine: e, source: source}, nil
}
func (r *preparedPlanReplanRuntime) ExecutePlanReplan(ctx context.Context, a coordination.PlanReplanAdmission) (err error) {
	e := r.engine
	ctx, trace := e.startRun(ctx, modeSync, false)
	var result *run.Run
	defer func() {
		if recovered := recover(); recovered != nil {
			trace.end(result, telemetry.ErrPanic)
			panic(recovered)
		}
		trace.end(result, err)
	}()
	if a.Validate() != nil || !a.Fresh {
		return planning.ErrConflict
	}
	storage, ok := e.store.(coordination.PlanReplanStore)
	if !ok {
		return planning.ErrInvalid
	}
	retained, err := storage.GetPlanReplan(ctx, a.Request.ID)
	if err != nil {
		return err
	}
	if retained == nil || retained.Validate() != nil || retained.RequestDigest != a.RequestDigest {
		return planning.ErrConflict
	}
	current, err := e.store.GetRun(ctx, a.Request.RunID)
	if err != nil && !errors.Is(err, cortex.ErrRunNotFound) {
		return err
	}
	if current != nil {
		return planning.ErrConflict
	}
	if !time.Now().Before(a.Request.Reservation.Deadline) {
		return planning.ErrConflict
	}
	bounded, cancel := context.WithDeadline(ctx, a.Request.Reservation.Deadline)
	defer cancel()
	request, _, err := a.Request.Freeze()
	if err != nil {
		return err
	}
	prepared, input, err := r.source.ResolvePlanReplanInput(bounded, request)
	if err != nil {
		return err
	}
	if prepared != nil {
		prepared = &PreparedAgent{payload: append([]byte(nil), prepared.payload...), digest: prepared.digest}
	}
	if prepared == nil || prepared.Digest() != a.Request.PreparedDigest || evidence.Digest([]byte(input)) != a.Request.InputDigest {
		return planning.ErrConflict
	}
	snapshot, err := prepared.snapshot()
	if err != nil {
		return err
	}
	budget := snapshot.Config.Budget
	allocated := a.Request.Reservation.Allocation
	if snapshot.Agent.ID != a.Request.AgentID || snapshot.Scope.Canonical() != a.Request.Progress.Plan.Scope.Canonical() || !snapshot.Config.AllToolsRestricted || !snapshot.Config.ToolsRestricted || len(snapshot.Tools) != 0 || len(snapshot.Config.Tools) != 0 || snapshot.Config.Verification != nil || snapshot.Config.Context != nil || budget.MaxSteps < 1 || int64(budget.MaxSteps) > allocated.Steps || budget.MaxTokens < 1 || int64(budget.MaxTokens) > allocated.Tokens || budget.Deadline.IsZero() || budget.Deadline.After(a.Request.Reservation.Deadline) {
		return planning.ErrInvalid
	}
	if sessionErr := e.store.CreateSession(bounded, &session.Session{ID: a.Request.SessionID, AgentID: a.Request.AgentID, Title: "Plan proposal"}); sessionErr != nil {
		return sessionErr
	}
	ctx, draft, err := e.prepareRun(bounded, prepared, input, PreparedRunOptions{SessionID: a.Request.SessionID, Purpose: run.PurposeEvaluation}, a.Request.RunID)
	if err != nil {
		return err
	}
	if evidence.Digest([]byte(draft.run.Input)) != a.Request.InputDigest {
		return planning.ErrConflict
	}
	// The insert-only session belongs to this admission and has no prior history.
	ctx, resumption, err := e.activatePrepared(ctx, draft)
	if err != nil {
		return err
	}
	result, err = e.continueReAct(ctx, resumption.ag, resumption.cfg, resumption.run, resumption.state, resumption.startedAt)
	return err
}
