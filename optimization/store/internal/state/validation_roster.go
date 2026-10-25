package state

import (
	"slices"

	"github.com/xraph/cortex/optimization"
)

// freezeValidationRoster derives assignments from stored child specs. It also
// rejects legacy cross-experiment allocations without a frozen parent roster.
func (tx *transaction) freezeValidationRoster(owner optimization.Experiment, input []optimization.ValidationArm) (roster []optimization.ValidationArm, tasks []optimization.TaskCase, err error) {
	if owner.Spec.Parent != nil {
		return nil, nil, optimization.ErrPolicyDenied
	}
	tasks = validationTasks(owner.Spec)
	if len(tasks) == 0 {
		return nil, nil, optimization.ErrInvalidSpec
	}
	plan := owner.Spec.Improvement
	if plan == nil {
		if len(input) != 1 || input[0].ExperimentID != owner.ID {
			return nil, nil, optimization.ErrPolicyDenied
		}
	} else if len(input) != len(plan.Children) {
		return nil, nil, optimization.ErrPolicyDenied
	}
	roster = append([]optimization.ValidationArm(nil), input...)
	covered := map[string]bool{}
	positions := map[string]int{}
	for index, arm := range roster {
		child, exists := tx.data.Experiments[arm.ExperimentID.String()]
		if !exists {
			return nil, nil, optimization.ErrNotFound
		}
		if plan != nil {
			binding := plan.Children[index]
			if binding.ExperimentID != child.ID || child.Spec.Parent == nil || !same(child.Spec.Parent, binding.Spec.Parent) || child.Spec.Parent.ExperimentID != owner.ID || child.Spec.Parent.Arm != index%2 || child.Spec.Parent.Sequence != index/2 || child.Spec.Controller != plan.Controllers[index%2] {
				return nil, nil, optimization.ErrPolicyDenied
			}
		}
		if child.Spec.AgentID != owner.Spec.AgentID || child.Spec.Controller != arm.Controller || child.Spec.Baseline != arm.Baseline || !same(child.Spec.Objective, owner.Spec.Objective) || child.Spec.Limits.Repetitions != owner.Spec.Limits.Repetitions || !same(child.EvaluationPlan.Scorers, owner.EvaluationPlan.Scorers) || !same(child.EvaluationPlan.Criteria, owner.EvaluationPlan.Criteria) {
			return nil, nil, optimization.ErrPolicyDenied
		}
		cases := validationTasks(child.Spec)
		if len(cases) == 0 {
			return nil, nil, optimization.ErrPolicyDenied
		}
		digests := make([]string, 0, len(cases))
		for _, task := range cases {
			if plan != nil {
				if sequence, exists := positions[task.InputDigest]; exists && sequence != index/2 {
					return nil, nil, optimization.ErrPolicyDenied
				}
				positions[task.InputDigest] = index / 2
			}
			if !slices.ContainsFunc(tasks, func(value optimization.TaskCase) bool { return same(value, task) }) {
				return nil, nil, optimization.ErrPolicyDenied
			}
			digests = append(digests, task.Digest)
			covered[task.Digest] = true
		}
		if arm.CaseDigests != nil && !slices.Equal(arm.CaseDigests, digests) {
			return nil, nil, optimization.ErrPolicyDenied
		}
		if plan != nil {
			roster[index].CaseDigests = digests
		} else {
			roster[index].CaseDigests = nil
		}
		if !tx.executionVersion(arm.Finalist, owner.Spec.AgentID) {
			return nil, nil, optimization.ErrStaleVersion
		}
		if arm.Finalist != child.Spec.Baseline {
			owned := false
			for _, candidate := range tx.data.Candidates {
				if candidate.ExperimentID == child.ID && candidate.Accepted && candidate.Version == arm.Finalist {
					owned = true
					break
				}
			}
			if !owned {
				return nil, nil, optimization.ErrNotFound
			}
		}
	}
	if len(covered) != len(tasks) {
		return nil, nil, optimization.ErrPolicyDenied
	}
	return roster, tasks, nil
}

func (tx *transaction) checkValidationRoster(allocation optimization.ValidationAllocation) error {
	owner, exists := tx.data.Experiments[allocation.ExperimentID.String()]
	if !exists {
		return optimization.ErrIncompleteEvidence
	}
	roster, _, err := tx.freezeValidationRoster(owner, allocation.PairedRoster)
	if err != nil {
		return err
	}
	if !same(roster, allocation.PairedRoster) {
		return optimization.ErrIncompleteEvidence
	}
	return nil
}
