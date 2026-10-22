package optimization

import (
	"context"
	"errors"
	"math"

	"github.com/xraph/cortex/llm"
)

// ModelObservation carries provider accounting facts without prompts or outputs.
// Completed describes execution, independently of whether usage is authoritative.
type ModelObservation struct {
	Reservation   UsageReservation
	RequestDigest string
	Model         string
	ModelRevision string
	ReportedModel string
	Streaming     bool
	Usage         llm.Usage
	UsagePresent  bool
	Completed     bool
	Err           error
}

// ModelUsageReport attributes usage to a trusted provider receipt. Complete means
// the accounting is complete, not that the model produced a successful answer.
type ModelUsageReport struct {
	Usage    Usage
	Complete bool
}

// ModelUsageReporter verifies provider usage and frozen pricing. Implementations
// must not infer authoritative zero usage from an absent report. Its work must
// not itself perform unmetered model calls or other charged operations.
type ModelUsageReporter interface {
	Observe(context.Context, ModelObservation) (ModelUsageReport, error)
}

func (call *modelCall) settle(usage *llm.Usage, reportedModel string, completed bool, cause error) error {
	tokens, usageErr := observedModelTokens(usage)
	observation := ModelObservation{Reservation: call.reservation, RequestDigest: call.digest, Model: call.client.config.Model, ModelRevision: call.client.config.ModelRevision, ReportedModel: reportedModel, Streaming: call.streaming, UsagePresent: usage != nil, Completed: completed, Err: cause}
	if usage != nil {
		observation.Usage = *usage
	}
	report := ModelUsageReport{Usage: Usage{Tokens: tokens, TokenQuality: UsageUnknown, Receipt: "model-observation/" + call.digest}}
	var reportErr error
	if reporter := call.client.reporter; reporter != nil && usageErr == nil && !errors.Is(cause, ErrIncompleteEvidence) && (reportedModel == "" || reportedModel == call.client.config.Model) {
		candidate, err := reporter.Observe(copyScopeContext(call.parent), observation)
		if err == nil {
			if candidate.Usage.Validate() != nil || !ValidOpaqueKey(candidate.Usage.Receipt) || candidate.Usage.Tokens < tokens || call.reservation.PricingRevision != "" && (candidate.Usage.MoneyQuality == UsageKnown || candidate.Usage.MoneyQuality == UsageEstimated) && candidate.Usage.PricingRevision != call.reservation.PricingRevision {
				err = ErrIncompleteEvidence
			}
		}
		if err != nil {
			reportErr = err
		} else {
			report = candidate
		}
	}
	settlement := UsageSettlement{ExperimentID: call.reservation.ExperimentID, CallKey: call.reservation.CallKey, AttemptID: call.reservation.AttemptID, Receipt: report.Usage.Receipt, Usage: report.Usage, Complete: report.Complete, Lease: call.reservation.Lease}
	settleErr := call.client.meter.Settle(copyScopeContext(call.parent), settlement)
	return errors.Join(usageErr, reportErr, settleErr)
}

func observedModelTokens(usage *llm.Usage) (int64, error) {
	if usage == nil {
		return 0, nil
	}
	var err error
	if usage.TotalTokens < 0 || usage.PromptTokens < 0 || usage.CompletionTokens < 0 {
		err = ErrIncompleteEvidence
	}
	prompt, completion, total := int64(max(0, usage.PromptTokens)), int64(max(0, usage.CompletionTokens)), int64(max(0, usage.TotalTokens))
	if prompt > math.MaxInt64-completion {
		return math.MaxInt64, ErrIncompleteEvidence
	}
	sum := prompt + completion
	if total > 0 && sum > 0 && total != sum {
		err = ErrIncompleteEvidence
	}
	return max(total, sum), err
}
