// Package execution defines cumulative execution limits and accounting.
package execution

import (
	"fmt"
	"math"
	"time"
)

// Budget bounds one run across steps and resumptions. Zero optional limits are
// disabled. MaxTokens gates further dispatch; provider-reported input usage can
// exceed the remaining allowance in one call and is never hidden or clamped.
type Budget struct {
	MaxSteps     int       `json:"max_steps,omitempty"`
	MaxTokens    int       `json:"max_tokens,omitempty"`
	MaxToolCalls int       `json:"max_tool_calls,omitempty"`
	Deadline     time.Time `json:"deadline,omitempty"`
}

// Usage is cumulative. Tokens includes conservative charges for missing usage;
// ReportedTokens retains the separately observed total.
type Usage struct {
	Steps          int  `json:"steps"`
	Tokens         int  `json:"tokens"`
	ReportedTokens int  `json:"reported_tokens"`
	ToolCalls      int  `json:"tool_calls"`
	Estimated      bool `json:"estimated"`
}

// Limit identifies which configured allowance prevents dispatch.
type Limit string

const (
	LimitSteps    Limit = "step_limit"
	LimitTokens   Limit = "token_limit"
	LimitTools    Limit = "tool_limit"
	LimitDeadline Limit = "deadline"
)

// Validate rejects negative limits, which cannot mean unlimited by accident.
func (b Budget) Validate() error {
	if b.MaxSteps < 0 || b.MaxTokens < 0 || b.MaxToolCalls < 0 {
		return fmt.Errorf("execution: limits must not be negative")
	}
	return nil
}

// Check reports whether another model step may start. Tool slots are checked
// separately, so a run can synthesize an answer after its final tool call.
func (b Budget) Check(u Usage, now time.Time) Limit {
	if !b.Deadline.IsZero() && !now.Before(b.Deadline) {
		return LimitDeadline
	}
	if b.MaxTokens > 0 && u.Tokens >= b.MaxTokens {
		return LimitTokens
	}
	if b.MaxSteps > 0 && u.Steps >= b.MaxSteps {
		return LimitSteps
	}
	return ""
}

// CheckTool reports whether a logical tool call may be admitted.
func (b Budget) CheckTool(u Usage, now time.Time) Limit {
	if !b.Deadline.IsZero() && !now.Before(b.Deadline) {
		return LimitDeadline
	}
	if b.MaxTokens > 0 && u.Tokens >= b.MaxTokens {
		return LimitTokens
	}
	if b.MaxToolCalls > 0 && u.ToolCalls >= b.MaxToolCalls {
		return LimitTools
	}
	return ""
}

// OutputLimit narrows the provider's per-call output cap to the remaining total.
func (b Budget) OutputLimit(configured int, u Usage) int {
	if b.MaxTokens <= 0 {
		return configured
	}
	remaining := b.MaxTokens - u.Tokens
	if remaining < 0 {
		return 0
	}
	if configured <= 0 || remaining < configured {
		return remaining
	}
	return configured
}

// ChargeTokens retains known usage. Unknown or invalid usage consumes all of a
// configured remaining token allowance, so missing accounting cannot reset it.
func (u *Usage) ChargeTokens(reported int, b Budget) {
	if reported > 0 {
		u.ReportedTokens = saturatingAdd(u.ReportedTokens, reported)
		u.Tokens = saturatingAdd(u.Tokens, reported)
		return
	}
	u.Estimated = true
	if b.MaxTokens > u.Tokens {
		u.Tokens = b.MaxTokens
	}
}

func saturatingAdd(a, b int) int {
	if a > math.MaxInt-b {
		return math.MaxInt
	}
	return a + b
}

// Equal compares persisted limits independently of timezone or monotonic clocks.
func (b Budget) Equal(other Budget) bool {
	return b.MaxSteps == other.MaxSteps && b.MaxTokens == other.MaxTokens && b.MaxToolCalls == other.MaxToolCalls && b.Deadline.Equal(other.Deadline)
}
