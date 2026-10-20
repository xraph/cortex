package verification

import "time"

// CriterionDecision retains contract ordering and explains missing checks.
type CriterionDecision struct {
	CriterionID string `json:"criterion_id"`
	Required    bool   `json:"required"`
	Status      Status `json:"status"`
}

// Decision is a derived verdict, not a persisted authorization to publish.
type Decision struct {
	Status   Status              `json:"status"`
	Criteria []CriterionDecision `json:"criteria"`
}

// Aggregate requires all required criteria for the same immutable binding.
// The caller must separately check that binding against current authorized
// state. Conflicting reports cannot be resolved by selecting a favorable one.
func Aggregate(contract Contract, reports []Report, at time.Time) Decision {
	decision := Decision{Status: StatusInconclusive}
	if contract.Validate() != nil || at.IsZero() || len(reports) > 256 {
		return decision
	}
	criteria := make(map[string]Criterion, len(contract.Criteria))
	for _, criterion := range contract.Criteria {
		criteria[criterion.ID] = criterion
	}
	outcomes := make(map[string]Status)
	bindingDigest := ""
	for _, report := range reports {
		if ValidateReportForContract(contract, report) != nil {
			return decision
		}
		digest, err := report.Binding.Digest()
		if err != nil {
			return decision
		}
		if bindingDigest != "" && bindingDigest != digest {
			return decision
		}
		bindingDigest = digest
		fresh := CheckFreshness(report, report.Binding, at)
		for _, outcome := range report.Outcomes {
			criterion := criteria[outcome.CriterionID]
			if _, exists := outcomes[outcome.CriterionID]; exists {
				return decision
			}
			status := outcome.Status
			if fresh != StatusPassed {
				status = fresh
			}
			if !report.CheckedAt.Add(criterion.MaxAge).After(at) {
				status = StatusStale
			}
			if criterion.MaxSourceAge > 0 {
				if len(report.Binding.Context.Sources) == 0 && status == StatusPassed {
					status = StatusInconclusive
				}
				for _, source := range report.Binding.Context.Sources {
					if !source.RetrievedAt.Add(criterion.MaxSourceAge).After(at) {
						status = StatusStale
					}
				}
			}
			if status == StatusPassed && len(outcome.EvidenceIDs) < criterion.MinEvidence {
				status = StatusInconclusive
			}
			if status == StatusPassed && criterion.Kind == KindHuman && outcome.Human == nil {
				status = StatusInconclusive
			}
			outcomes[outcome.CriterionID] = status
		}
	}
	decision.Status = StatusPassed
	for _, criterion := range contract.Criteria {
		status, ok := outcomes[criterion.ID]
		if !ok {
			status = StatusInconclusive
		}
		decision.Criteria = append(decision.Criteria, CriterionDecision{CriterionID: criterion.ID, Required: criterion.Required, Status: status})
		if criterion.Required {
			decision.Status = combineStatus(decision.Status, status)
		}
	}
	return decision
}

func combineStatus(current, next Status) Status {
	if current == StatusFailed || next == StatusFailed {
		return StatusFailed
	}
	if current == StatusStale || next == StatusStale {
		return StatusStale
	}
	if current != StatusPassed || next != StatusPassed {
		return StatusInconclusive
	}
	return StatusPassed
}
