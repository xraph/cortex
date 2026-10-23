package run

import (
	"errors"

	"github.com/xraph/cortex"
)

// Public verification failure codes are stable across transport adapters.
const (
	FailureUnverified              = "run_unverified"
	FailureVerificationUnavailable = "verification_unavailable"
)

// VerificationFailure contains safe metadata for inspecting a failed request.
// Retryable is false: a new run can repeat generation or external tool effects.
// An unavailable verifier may already have performed durable work; recover that
// work through its saved identities rather than automatically starting again.
type VerificationFailure struct {
	Code               string `json:"code"`
	Message            string `json:"message"`
	RunID              string `json:"run_id,omitempty"`
	State              State  `json:"state,omitempty"`
	VerificationStatus string `json:"verification_status,omitempty"`
	Retryable          bool   `json:"retryable"`
}

// DescribeVerificationFailure classifies wrapped sentinels without preserving
// private diagnostics. A run supplies identity and status only, never its output.
func DescribeVerificationFailure(err error, r *Run) (VerificationFailure, bool) {
	var failure VerificationFailure
	switch {
	case errors.Is(err, cortex.ErrRunUnverified):
		failure.Code, failure.Message = FailureUnverified, cortex.ErrRunUnverified.Error()
	case errors.Is(err, cortex.ErrVerificationUnavailable):
		failure.Code, failure.Message = FailureVerificationUnavailable, cortex.ErrVerificationUnavailable.Error()
	default:
		return failure, false
	}
	if r != nil && !r.ID.IsNil() {
		failure.RunID, failure.State = r.ID.String(), r.State
		failure.VerificationStatus = string(r.VerificationStatus.Normalized())
	}
	return failure, true
}

// Fields returns a detached, transport-neutral error payload. Missing run
// identity means this response cannot identify saved work, not that none exists.
func (f VerificationFailure) Fields() map[string]any {
	fields := map[string]any{"code": f.Code, "message": f.Message, "retryable": f.Retryable}
	if f.RunID != "" {
		fields["run_id"], fields["state"], fields["verification_status"] = f.RunID, string(f.State), f.VerificationStatus
	}
	return fields
}
