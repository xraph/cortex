package run

import (
	"time"

	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/verification"
)

// StopReason records why execution ended. Empty means historical or unknown.
type StopReason string

const (
	StopExecutionError     StopReason = "execution_error"
	StopInvalidResponse    StopReason = "invalid_response"
	StopProviderUnknown    StopReason = "provider_unknown"
	StopFinalAnswer        StopReason = "final_answer"
	StopStepLimit          StopReason = "step_limit"
	StopToolLimit          StopReason = "tool_limit"
	StopTokenLimit         StopReason = "token_limit"
	StopCostLimit          StopReason = "cost_limit"
	StopDeadline           StopReason = "deadline"
	StopCancelled          StopReason = "cancelled"
	StopSafetyBlock        StopReason = "safety_block"
	StopPolicyUnavailable  StopReason = "policy_unavailable"
	StopToolError          StopReason = "tool_error"
	StopPersistenceError   StopReason = "persistence_error"
	StopVerificationFailed StopReason = "verification_failed"
)

// Outcome is an execution result, not a verification claim.
type Outcome struct {
	Transcript   *verification.ExecutionTranscript `json:"-"`
	StopReason   StopReason
	Output       string
	FinishReason string
	StepCount    int
	TokensUsed   int
	CompletedAt  time.Time
}

// TerminalSummary contains bounded outcome metadata without model or tool text.
// It describes a persisted execution, independently of factual verification.
type TerminalSummary struct {
	State              State                    `json:"state"`
	StopReason         StopReason               `json:"stop_reason"`
	VerificationStatus verification.Status      `json:"verification_status"`
	PublicationStatus  safety.PublicationStatus `json:"publication_status"`
}

// TerminalSummary returns outcome metadata with legacy verification normalized.
func (r *Run) TerminalSummary() TerminalSummary {
	return TerminalSummary{State: r.State, StopReason: r.StopReason, VerificationStatus: r.VerificationStatus.Normalized(), PublicationStatus: r.Provenance.PublicationStatus}
}
