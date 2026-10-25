package optimization

import (
	"math"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization/internal/canonical"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
)

// OrchestrationExecution retains the original team, closed account and every
// reserved child. A failed team may retain reservations without child captures.
// Receipt integrity is checked here; the environment authenticates their source.
type OrchestrationExecution struct {
	Run      orchestration.Run             `json:"run"`
	Budget   orchestration.BudgetSnapshot  `json:"budget"`
	Children []OrchestrationChildExecution `json:"children"`
}

// OrchestrationChildExecution binds one original reservation to its captured run.
// Execution is absent when dispatch or capture failed before a terminal result.
type OrchestrationChildExecution struct {
	Reservation execution.Reservation `json:"reservation"`
	Execution   *ExecutionRecord      `json:"execution,omitempty"`
}

// ExecutionID returns the original agent or team identity without changing its prefix.
func (record ExecutionRecord) ExecutionID() id.ID {
	if record.Orchestration != nil {
		return record.Orchestration.Run.ID
	}
	return record.RunID
}

// OrchestrationChildUsage reads measured dimensions from a host-authenticated
// settlement. Missing receipts remain unknown; reserved capacity is never usage.
func OrchestrationChildUsage(policy orchestration.BudgetPolicy, reservation execution.Reservation) (Usage, error) {
	usage := Usage{TokenQuality: UsageUnknown, MoneyQuality: UsageUnknown}
	if reservation.Validate() != nil || policy.ValidateReservation(reservation.Request) != nil {
		return Usage{}, ErrIncompleteEvidence
	}
	if reservation.Settlement == nil {
		return usage, nil
	}
	settlement := reservation.Settlement.Request
	if settlement.CloseBranch {
		return Usage{}, ErrIncompleteEvidence
	}
	usage.Tokens = settlement.Usage.Actual.Tokens
	usage.USDMicros = settlement.Usage.Actual.USDMicros
	if settlement.Minimum != nil {
		usage.Tokens = max(usage.Tokens, settlement.Minimum.Tokens)
		usage.USDMicros = max(usage.USDMicros, settlement.Minimum.USDMicros)
	}
	usage.Receipt = settlement.Receipt
	if settlement.Usage.Known&execution.KnownTokens != 0 {
		usage.TokenQuality = UsageKnown
	}
	if settlement.Usage.Known&execution.KnownMoney != 0 && !policy.TokenOnly {
		usage.MoneyQuality = UsageKnown
		usage.PricingRevision = policy.PricingRevision
	}
	if usage.TokenQuality == UsageKnown || usage.MoneyQuality == UsageKnown {
		if usage.Receipt == "" {
			return Usage{}, ErrIncompleteEvidence
		}
	}
	return usage, nil
}

// OrchestrationUsage aggregates original receipts, retaining unknown dimensions
// and observed lower bounds. It never reads BudgetSnapshot.Charged as usage.
func OrchestrationUsage(capture OrchestrationExecution) (Usage, error) {
	result := Usage{TokenQuality: UsageKnown, MoneyQuality: UsageKnown}
	if _, _, err := capture.Budget.Policy.Freeze(); err != nil || capture.Budget.Reservations != len(capture.Children) {
		return Usage{}, ErrIncompleteEvidence
	}
	if len(capture.Children) == 0 {
		return Usage{TokenQuality: UsageUnknown, MoneyQuality: UsageUnknown}, nil
	}
	if len(capture.Children) > execution.MaxReservations || capture.Budget.Reservations != len(capture.Children) {
		return Usage{}, ErrIncompleteEvidence
	}
	receipts := make([]execution.Reservation, 0, len(capture.Children))
	seen := map[id.ID]bool{}
	for _, child := range capture.Children {
		if seen[child.Reservation.Request.ID] {
			return Usage{}, ErrIncompleteEvidence
		}
		seen[child.Reservation.Request.ID] = true
		usage, err := OrchestrationChildUsage(capture.Budget.Policy, child.Reservation)
		if err != nil {
			return Usage{}, err
		}
		if result.Tokens > math.MaxInt64-usage.Tokens {
			result.Tokens = math.MaxInt64
			result.TokenQuality = UsageUnknown
		} else {
			result.Tokens += usage.Tokens
		}
		if result.USDMicros > math.MaxInt64-usage.USDMicros {
			result.USDMicros = math.MaxInt64
			result.MoneyQuality = UsageUnknown
		} else {
			result.USDMicros += usage.USDMicros
		}
		if usage.TokenQuality != UsageKnown {
			result.TokenQuality = UsageUnknown
		}
		if usage.MoneyQuality != UsageKnown {
			result.MoneyQuality = UsageUnknown
		}
		receipts = append(receipts, child.Reservation)
	}
	if result.MoneyQuality == UsageKnown {
		result.PricingRevision = capture.Budget.Policy.PricingRevision
	}
	data, err := canonical.Marshal(receipts)
	if err != nil {
		return Usage{}, err
	}
	result.Receipt = "orchestration/" + digestBytes(data)
	return result, nil
}

func validateOrchestrationCapture(record ExecutionRecord) error {
	capture := record.Orchestration
	team := capture.Run
	policy := capture.Budget.Policy
	if !record.RunID.IsNil() || len(record.StepIDs) != 0 || len(record.ToolCallIDs) != 0 || team.ID.Prefix() != id.PrefixOrchestration || team.ConfigID.Prefix() != id.PrefixOrchestrationConfig || team.Strategy == "" || team.CompletedAt == nil || !team.StartedAt.Equal(record.StartedAt) || !team.CompletedAt.Equal(record.CompletedAt) || team.Output != record.Outcome.Output || digestBytes([]byte(team.Input)) != record.InputDigest || team.Scope.Canonical() != record.Environment.Scope.Canonical() {
		return ErrIncompleteEvidence
	}
	if _, _, err := policy.Freeze(); err != nil {
		return ErrIncompleteEvidence
	}
	if !capture.Budget.Closed || capture.Budget.Reservations != len(capture.Children) || len(capture.Children) > execution.MaxReservations || capture.Budget.Charged.Validate() != nil || policy.ExecutionID != team.ID || policy.PreparedDigest != record.PreparedDigest || policy.Scope.Canonical() != team.Scope.Canonical() || len(policy.ParticipantDigests) != len(policy.Participants) {
		return ErrIncompleteEvidence
	}
	switch team.Status {
	case orchestration.StatusCompleted:
		if record.State != run.StateCompleted || len(capture.Children) == 0 {
			return ErrIncompleteEvidence
		}
	case orchestration.StatusFailed:
		if record.State != run.StateFailed && record.State != run.StateCancelled && record.State != run.StateIncomplete {
			return ErrIncompleteEvidence
		}
	default:
		return ErrIncompleteEvidence
	}
	linked := map[id.ID]bool{}
	for _, key := range team.AgentRunIDs {
		if key.Prefix() != id.PrefixAgentRun || linked[key] {
			return ErrIncompleteEvidence
		}
		linked[key] = true
	}
	seen := map[id.ID]bool{}
	charged := execution.Resources{}
	overflow := false
	for _, child := range capture.Children {
		reservation := child.Reservation
		key := reservation.Request.TaskID
		usage, err := OrchestrationChildUsage(policy, reservation)
		if err != nil {
			return err
		}
		if reservation.CreatedAt.Before(record.StartedAt) || reservation.CreatedAt.After(record.CompletedAt) {
			return ErrIncompleteEvidence
		}
		charge := reservation.Request.Allocation
		if reservation.Settlement != nil {
			charge, err = execution.SettlementCharge(reservation.Request, reservation.Settlement.Request)
			if err != nil {
				return ErrIncompleteEvidence
			}
			overflow = overflow || reservation.Settlement.Request.Overflow != 0
		}
		var sumOverflow bool
		charged, sumOverflow = charged.Add(charge)
		overflow = overflow || sumOverflow
		if seen[key] {
			return ErrIncompleteEvidence
		}
		seen[key] = true
		if child.Execution == nil {
			if record.State == run.StateCompleted {
				return ErrIncompleteEvidence
			}
			continue
		}
		r := child.Execution
		if r.Orchestration != nil || !linked[key] || r.RunID != key || r.PreparedDigest != policy.ParticipantDigests[reservation.Request.OwnerRef] || !equalControllerData(r.Environment, record.Environment) || r.StartedAt.Before(record.StartedAt) || r.CompletedAt.After(record.CompletedAt) || !equalControllerData(r.Usage, usage) {
			return ErrIncompleteEvidence
		}
		sealed, sealErr := SealExecution(*r)
		if sealErr != nil || sealed.Digest != r.Digest {
			return ErrIncompleteEvidence
		}
	}
	if charged != capture.Budget.Charged || overflow != capture.Budget.Overflow {
		return ErrIncompleteEvidence
	}
	for key := range linked {
		if !seen[key] {
			return ErrIncompleteEvidence
		}
	}
	usage, err := OrchestrationUsage(*capture)
	if err != nil || !equalControllerData(record.Usage, usage) {
		return ErrIncompleteEvidence
	}
	return nil
}

// ValidateExecutionVersion binds a sealed capture to its exact prepared version.
func ValidateExecutionVersion(record ExecutionRecord, version HarnessVersion) error {
	sealed, err := SealExecution(record)
	if err != nil || sealed.Digest != record.Digest {
		return ErrIncompleteEvidence
	}
	frozen, err := SealVersion(version)
	if err != nil || frozen.Digest != version.Digest || record.Environment.Scope.Canonical() != version.Scope.Canonical() {
		return ErrIncompleteEvidence
	}
	if version.Orchestration == nil {
		if record.Orchestration != nil || version.Prepared == nil || record.PreparedDigest != version.Prepared.Digest() {
			return ErrIncompleteEvidence
		}
		return nil
	}
	if record.Orchestration == nil || record.PreparedDigest != version.Orchestration.Prepared.Digest() {
		return ErrIncompleteEvidence
	}
	capture := record.Orchestration
	view, err := version.Orchestration.Prepared.Inspect()
	if err != nil {
		return ErrIncompleteEvidence
	}
	preset, err := orchestrationPreset(version.Policy, version.Orchestration.Preset)
	if err != nil {
		return err
	}
	policy := capture.Budget.Policy
	if capture.Run.ConfigID != view.Config.ID || capture.Run.Strategy != view.Config.Strategy || !preset.Limits.Covers(policy.Limits) || len(policy.Participants) != len(view.Participants) {
		return ErrIncompleteEvidence
	}
	for name, prepared := range view.Participants {
		child, inspectErr := prepared.Inspect()
		if inspectErr != nil || !participantResources(child).Covers(policy.Participants[name]) {
			return ErrIncompleteEvidence
		}
		if policy.ParticipantDigests[name] != prepared.Digest() {
			return ErrIncompleteEvidence
		}
	}
	return nil
}

// ExecutionCompleted distinguishes fully completed captures from partial team
// results. This describes execution completeness, not answer correctness.
func ExecutionCompleted(record ExecutionRecord) bool {
	if record.State != run.StateCompleted {
		return false
	}
	if record.Orchestration != nil {
		for _, child := range record.Orchestration.Children {
			if child.Execution == nil || child.Execution.State != run.StateCompleted {
				return false
			}
		}
	}
	return true
}

func participantResources(child engine.PreparedView) execution.Resources {
	budget := child.Config.Budget
	tokens := budget.MaxTokens
	if child.Config.Verification != nil {
		tokens = child.Config.Verification.TotalTokens
	}
	return execution.Resources{Steps: int64(budget.MaxSteps), Tokens: int64(tokens), ToolCalls: int64(budget.MaxToolCalls), FanOut: 1}
}
