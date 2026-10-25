package state

import "github.com/xraph/cortex/optimization"

func (tx *transaction) validateFamily(bundle optimization.ExperimentBundle) error {
	e := bundle.Experiment
	if e.Spec.Improvement != nil {
		for _, child := range e.Spec.Improvement.Children {
			if _, exists := tx.data.Experiments[child.ExperimentID.String()]; exists {
				return optimization.ErrConflict
			}
			if child.ExperimentID == e.ID || child.Spec.Parent.ExperimentID != e.ID {
				return optimization.ErrInvalidSpec
			}
		}
	}
	if e.Spec.Parent == nil {
		if bundle.ParentLease != nil {
			return optimization.ErrInvalidSpec
		}
		return nil
	}
	parent, ok := tx.data.Experiments[e.Spec.Parent.ExperimentID.String()]
	if !ok || parent.Spec.Parent != nil || parent.Spec.Improvement == nil {
		return optimization.ErrInvalidSpec
	}
	if bundle.ParentLease == nil || !tx.validLease(parent, *bundle.ParentLease) {
		return optimization.ErrConflict
	}
	for _, child := range parent.Spec.Improvement.Children {
		if child.ExperimentID != e.ID {
			continue
		}
		if !same(child.Spec, e.Spec) || !same(child.EvaluationPlan, e.EvaluationPlan) || !same(parent.LessonIDs, e.LessonIDs) {
			return optimization.ErrConflict
		}
		return nil
	}
	return optimization.ErrPolicyDenied
}

func (tx *transaction) liveParent(e optimization.Experiment) (optimization.Experiment, error) {
	if e.Spec.Parent == nil {
		return optimization.Experiment{}, nil
	}
	parent, exists := tx.data.Experiments[e.Spec.Parent.ExperimentID.String()]
	if !exists || parent.Spec.Parent != nil || parent.Spec.Improvement == nil || !tx.validLease(parent, parent.Lease) {
		return optimization.Experiment{}, optimization.ErrConflict
	}
	return parent, nil
}

// checkFamilyBudget subtracts original debits from each ceiling. This retains
// unknown usage and detects overruns even when their sum exceeds MaxInt64.
func (tx *transaction) checkFamilyBudget(e optimization.Experiment, extraTokens, extraMoney int64) error {
	b := e.Spec.Budget
	tokens, money := b.MaxTokens, b.MaxUSDMicros
	for _, key := range budgetMembers(e) {
		for _, entry := range tx.data.Reservations[key] {
			if entry.DebitedTokens > tokens || !b.TokenOnly && entry.DebitedUSDMicros > money {
				return optimization.ErrBudgetExceeded
			}
			tokens -= entry.DebitedTokens
			if !b.TokenOnly {
				money -= entry.DebitedUSDMicros
			}
		}
	}
	if extraTokens > tokens || !b.TokenOnly && extraMoney > money {
		return optimization.ErrBudgetExceeded
	}
	if e.Spec.Parent != nil {
		parent, err := tx.liveParent(e)
		if err != nil {
			return err
		}
		return tx.checkFamilyBudget(parent, extraTokens, extraMoney)
	}
	return nil
}

func budgetMembers(e optimization.Experiment) []string {
	keys := []string{e.ID.String()}
	if e.Spec.Improvement != nil {
		for _, child := range e.Spec.Improvement.Children {
			keys = append(keys, child.ExperimentID.String())
		}
	}
	return keys
}

func (tx *transaction) familyReceiptConflict(e optimization.Experiment, receipt, callKey string) bool {
	parent := e
	if e.Spec.Parent != nil {
		parent = tx.data.Experiments[e.Spec.Parent.ExperimentID.String()]
	}
	for _, key := range budgetMembers(parent) {
		if call, ok := tx.data.Receipts[key][receipt]; ok && (key != e.ID.String() || call != callKey) {
			return true
		}
	}
	return false
}
