package assurance

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
)

type preparedProposalBinding struct {
	RunID          id.AgentRunID
	SessionID      id.SessionID
	AgentID        id.AgentID
	Scope          cortex.Scope
	PreparedDigest string
	InputDigest    string
	NotBefore      time.Time
	Reservation    execution.ReservationRequest
}

// validatePreparedProposalRun checks one locked, isolated model run. The caller
// must separately establish terminality and authenticate retained observations.
func validatePreparedProposalRun(binding preparedProposalBinding, r *run.Run) error {
	if r == nil {
		return planning.ErrConflict
	}
	budget, allowance := r.Budget, binding.Reservation.Allocation
	if r.ID != binding.RunID || r.SessionID != binding.SessionID || r.AgentID != binding.AgentID || r.Scope.Canonical() != binding.Scope.Canonical() || r.Provenance.PreparedDigest != binding.PreparedDigest || evidence.Digest([]byte(r.Input)) != binding.InputDigest || r.Provenance.VerificationRequired || r.Provenance.VerificationIntentID != "" || r.Provenance.ContextSelection != nil || r.Provenance.OrchestrationID != "" || r.Provenance.TaskClaimID != "" || r.Provenance.Purpose != run.PurposeEvaluation || len(r.Provenance.Tools) != 0 || r.Usage.ToolCalls != 0 || r.StartedAt == nil || r.StartedAt.Before(binding.NotBefore) || budget.MaxSteps < 1 || int64(budget.MaxSteps) > allowance.Steps || budget.MaxTokens < 1 || int64(budget.MaxTokens) > allowance.Tokens || budget.Deadline.IsZero() || budget.Deadline.After(binding.Reservation.Deadline) || r.Usage.Steps < 0 || r.Usage.Tokens < 0 || r.Usage.ReportedTokens < 0 || r.StepCount < 0 || r.TokensUsed < 0 {
		return planning.ErrConflict
	}
	return nil
}

// preparedProposalUsage preserves model overages while callers retain unknown
// tokens and money conservatively from the full reservation allocation.
func preparedProposalUsage(r *run.Run) execution.Resources {
	return execution.Resources{Steps: int64(max(r.Usage.Steps, r.StepCount)), Tokens: int64(max(r.Usage.Tokens, r.Usage.ReportedTokens, r.TokensUsed)), FanOut: 1}
}
