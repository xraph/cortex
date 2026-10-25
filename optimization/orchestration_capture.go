package optimization

import (
	"context"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
)

// OrchestrationCaptureSource reads original runtime evidence. The host supplies
// its isolated store and authenticates settlements before they reach that store.
type OrchestrationCaptureSource interface {
	GetOrchestrationRun(context.Context, id.OrchestrationID) (*orchestration.Run, error)
	GetOrchestrationBudgetSnapshot(context.Context, id.OrchestrationID) (orchestration.BudgetSnapshot, error)
	ListOrchestrationReservations(context.Context, execution.ReservationListRequest) ([]execution.Reservation, error)
	GetRun(context.Context, id.AgentRunID) (*run.Run, error)
	ListSteps(context.Context, id.AgentRunID) ([]*run.Step, error)
	ListToolCalls(context.Context, id.StepID) ([]*run.ToolCall, error)
}

// CaptureOrchestration reads a terminal team's original evidence without running
// or resuming it. It does not qualify isolation or authenticate provider billing.
func CaptureOrchestration(ctx context.Context, source OrchestrationCaptureSource, version HarnessVersion, receipt EnvironmentReceipt, key id.OrchestrationID) (ExecutionRecord, error) {
	var empty ExecutionRecord
	if source == nil || receipt.Validate() != nil || cortex.ScopeFromContext(ctx).Canonical() != receipt.Scope.Canonical() || version.Orchestration == nil {
		return empty, ErrInvalidSpec
	}
	team, err := source.GetOrchestrationRun(ctx, key)
	if err != nil {
		return empty, err
	}
	if team == nil || team.ID != key || team.CompletedAt == nil || (team.Status != orchestration.StatusCompleted && team.Status != orchestration.StatusFailed) {
		return empty, ErrUnknownAttempt
	}
	capture := OrchestrationExecution{Run: *team}
	query := execution.ReservationListRequest{PlanID: key, Limit: 100}
	for {
		page, listErr := source.ListOrchestrationReservations(ctx, query)
		if listErr != nil {
			return empty, listErr
		}
		if len(page) == 0 {
			break
		}
		for _, reservation := range page {
			if reservation.Request.ID.String() <= query.After.String() && !query.After.IsNil() || len(capture.Children) >= execution.MaxReservations {
				return empty, ErrIncompleteEvidence
			}
			capture.Children = append(capture.Children, OrchestrationChildExecution{Reservation: reservation})
			query.After = reservation.Request.ID
		}
	}
	capture.Budget, err = source.GetOrchestrationBudgetSnapshot(ctx, key)
	if err != nil {
		return empty, err
	}
	for i := range capture.Children {
		child := &capture.Children[i]
		child.Execution, err = captureOrchestrationChild(ctx, source, version, receipt, capture.Budget.Policy, child.Reservation)
		if err != nil {
			return empty, err
		}
	}
	state, stop := run.StateCompleted, run.StopFinalAnswer
	if team.Status == orchestration.StatusFailed {
		state, stop = run.StateFailed, run.StopExecutionError
	}
	record := ExecutionRecord{Orchestration: &capture, PreparedDigest: version.Orchestration.Prepared.Digest(), InputDigest: digestBytes([]byte(team.Input)), State: state, Outcome: run.Outcome{Output: team.Output, StopReason: stop, CompletedAt: *team.CompletedAt}, StartedAt: team.StartedAt, CompletedAt: *team.CompletedAt, Duration: team.CompletedAt.Sub(team.StartedAt), Environment: receipt}
	record.Usage, err = OrchestrationUsage(capture)
	if err != nil {
		return empty, err
	}
	record, err = SealExecution(record)
	if err != nil {
		return empty, err
	}
	if validationErr := ValidateExecutionVersion(record, version); validationErr != nil {
		return empty, validationErr
	}
	return record, nil
}

func captureOrchestrationChild(ctx context.Context, source OrchestrationCaptureSource, version HarnessVersion, receipt EnvironmentReceipt, policy orchestration.BudgetPolicy, reservation execution.Reservation) (*ExecutionRecord, error) {
	r, err := source.GetRun(ctx, reservation.Request.TaskID)
	if errors.Is(err, cortex.ErrRunNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if r == nil || r.ID != reservation.Request.TaskID || r.Scope.Canonical() != receipt.Scope.Canonical() || r.Provenance.Purpose != run.PurposeOptimizationTrial || r.Provenance.OrchestrationID != policy.ExecutionID.String() || r.Provenance.OrchestrationDigest != policy.PreparedDigest || r.Provenance.OrchestrationReservationID != reservation.Request.ID.String() || r.Provenance.PreparedDigest != policy.ParticipantDigests[reservation.Request.OwnerRef] || r.Provenance.EnvironmentReceiptID != receipt.ID || r.Provenance.HarnessID != version.HarnessID.String() || r.Provenance.HarnessVersion != version.ID.String() {
		return nil, ErrIncompleteEvidence
	}
	if r.CompletedAt == nil || r.StartedAt == nil {
		return nil, nil
	}
	record := ExecutionRecord{RunID: r.ID, PreparedDigest: r.Provenance.PreparedDigest, InputDigest: digestBytes([]byte(r.Input)), State: r.State, Outcome: run.Outcome{Output: r.Output, FinishReason: r.FinishReason, StopReason: r.StopReason, StepCount: r.StepCount, TokensUsed: r.TokensUsed, CompletedAt: *r.CompletedAt}, StartedAt: *r.StartedAt, CompletedAt: *r.CompletedAt, Duration: r.CompletedAt.Sub(*r.StartedAt), Environment: receipt}
	record.Usage, err = OrchestrationChildUsage(policy, reservation)
	if err != nil {
		return nil, err
	}
	steps, err := source.ListSteps(ctx, r.ID)
	if err != nil {
		return nil, err
	}
	for _, step := range steps {
		if step == nil || step.RunID != r.ID || step.Scope.Canonical() != r.Scope.Canonical() {
			return nil, ErrIncompleteEvidence
		}
		record.StepIDs = append(record.StepIDs, step.ID)
		calls, callErr := source.ListToolCalls(ctx, step.ID)
		if callErr != nil {
			return nil, callErr
		}
		for _, call := range calls {
			if call == nil || call.StepID != step.ID || call.Scope.Canonical() != r.Scope.Canonical() {
				return nil, ErrIncompleteEvidence
			}
			record.ToolCallIDs = append(record.ToolCallIDs, call.ID)
		}
	}
	record, err = SealExecution(record)
	if err != nil {
		return nil, err
	}
	return &record, nil
}
