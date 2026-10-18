package run

import "time"

// StopReason records why execution ended. Empty means historical or unknown.
type StopReason string

const (
	StopExecutionError     StopReason = "execution_error"
	StopInvalidResponse    StopReason = "invalid_response"
	StopProviderUnknown    StopReason = "provider_unknown"
	StopFinalAnswer        StopReason = "final_answer"
	StopStepLimit          StopReason = "step_limit"
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
	StopReason   StopReason
	Output       string
	FinishReason string
	StepCount    int
	TokensUsed   int
	CompletedAt  time.Time
}
