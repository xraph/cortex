package optimization

import (
	"context"
	"errors"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
)

// ActiveOrchestrationRequest carries input for an explicitly reviewed team route.
// Shared capacity and participant settings come from its frozen approved preset.
type ActiveOrchestrationRequest struct {
	AgentID id.AgentID `json:"agent_id"`
	Channel string     `json:"channel"`
	Input   string     `json:"input"`
}

// RunActiveOrchestration executes the reviewed team under its approved shared budget.
func (o *Optimizer) RunActiveOrchestration(ctx context.Context, request ActiveOrchestrationRequest) (*orchestration.Run, error) {
	bound, err := o.resolveActiveExecution(ctx, request.AgentID, request.Channel, true)
	if err != nil {
		return nil, err
	}
	defer bound.release()
	version := bound.version
	deadline, _ := ctx.Deadline()
	policy, err := orchestrationExecutionBudget(version, id.NewOrchestrationID(), deadline)
	if err != nil {
		return nil, err
	}
	if !time.Now().Before(policy.Deadline) {
		return nil, ErrStaleVersion
	}
	result, err := o.activeEngine.RunPreparedOrchestration(copyScopeContext(ctx), version.Orchestration.Prepared, request.Input, engine.PreparedRunOptions{Purpose: run.PurposeNormal, OrchestrationBudget: &policy, Provenance: run.ExecutionProvenance{ExperimentID: bound.decision.ExperimentID.String(), HarnessID: version.HarnessID.String(), HarnessVersion: version.ID.String()}})
	if errors.Is(err, cortex.ErrInvalidPrepared) {
		return result, errors.Join(ErrStaleVersion, err)
	}
	return result, err
}
