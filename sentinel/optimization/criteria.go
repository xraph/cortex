package optimization

import (
	"slices"

	"github.com/xraph/sentinel/evalplan"

	opt "github.com/xraph/cortex/optimization"
)

func criterionRequiresExpected(c evalplan.Criterion) bool {
	if c.RequiresExpected || c.Scorer == "exact" {
		return true
	}
	if c.Scorer == "contains" || c.Scorer == "not_contains" {
		substring, ok := c.Config["substring"].(string)
		return !ok || substring == ""
	}
	return false
}

func mergeCriteria(current, next []opt.CriterionDefinition) ([]opt.CriterionDefinition, error) {
	if current != nil && len(current) != len(next) {
		return nil, opt.ErrIncompleteEvidence
	}
	merged := slices.Clone(next)
	for i, criterion := range next {
		if criterion.RequiresExpected == nil {
			return nil, opt.ErrIncompleteEvidence
		}
		required := *criterion.RequiresExpected
		if current != nil {
			previous := current[i]
			if previous.ID != criterion.ID || previous.Required != criterion.Required || previous.RequiresExpected == nil {
				return nil, opt.ErrIncompleteEvidence
			}
			required = required || *previous.RequiresExpected
		}
		merged[i].RequiresExpected = &required
	}
	return merged, nil
}

// Legacy offline plans omitted this capability. Preserve their bindings without
// treating absence as permission for task-local selection, which rejects nil.
func compatiblePlan(current, requested opt.EvaluationPlan) bool {
	current.Criteria = slices.Clone(current.Criteria)
	for i := range current.Criteria {
		for _, criterion := range requested.Criteria {
			if criterion.ID == current.Criteria[i].ID && criterion.RequiresExpected == nil {
				current.Criteria[i].RequiresExpected = nil
			}
		}
	}
	return equal(current, requested)
}
