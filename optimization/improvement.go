package optimization

import (
	"bytes"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// ParentExperiment binds a child to one position in a frozen controller comparison.
type ParentExperiment struct {
	ExperimentID id.ID `json:"experiment_id"`
	Arm          int   `json:"arm"`
	Sequence     int   `json:"sequence"`
}

// ImprovementChild retains the complete child template for restartable scheduling.
// Templates cannot contain another improvement plan.
type ImprovementChild struct {
	ExperimentID   id.ID          `json:"experiment_id"`
	Spec           ExperimentSpec `json:"spec"`
	EvaluationPlan EvaluationPlan `json:"evaluation_plan"`
}

// ImprovementPlan freezes two controller arms and their ordered child templates.
// Children are stored as consecutive pairs, baseline arm followed by candidate arm.
type ImprovementPlan struct {
	Controllers [2]VersionRef      `json:"controllers"`
	Children    []ImprovementChild `json:"children"`
}

func validateImprovement(spec ExperimentSpec) error {
	if spec.Parent != nil {
		p := spec.Parent
		if _, err := ParseID(p.ExperimentID.String(), PrefixExperiment); err != nil {
			return err
		}
		if p.Arm < 0 || p.Arm > 1 || p.Sequence < 0 || spec.Improvement != nil {
			return ErrInvalidSpec
		}
	}
	plan := spec.Improvement
	if plan == nil {
		return nil
	}
	if spec.Mode == ModeTaskLocal || len(plan.Children) == 0 || len(plan.Children)%2 != 0 || len(plan.Children) > spec.Limits.MaxTrials || plan.Controllers[0] != spec.Controller || plan.Controllers[0].ID == plan.Controllers[1].ID {
		return ErrInvalidSpec
	}
	registered := make(map[string]TaskCase, len(spec.Tasks))
	for _, task := range spec.Tasks {
		registered[task.Digest] = task
	}
	covered := map[string]bool{}
	positions := map[string]int{}
	seen := map[string]bool{}
	total := 0
	for i, child := range plan.Children {
		if _, err := ParseID(child.ExperimentID.String(), PrefixExperiment); err != nil {
			return err
		}
		if seen[child.ExperimentID.String()] {
			return ErrInvalidSpec
		}
		seen[child.ExperimentID.String()] = true
		c, p := child.Spec, child.Spec.Parent
		if c.Improvement != nil || p == nil || p.Arm != i%2 || p.Sequence != i/2 || c.Controller != plan.Controllers[i%2] || c.Scope.Canonical() != spec.Scope.Canonical() || c.AgentID != spec.AgentID || c.Requester != spec.Requester || c.Baseline != spec.Baseline {
			return ErrInvalidSpec
		}
		if c.Mode == ModeTaskLocal || c.Limits.Repetitions != spec.Limits.Repetitions || !equalImprovementTemplate(c.Objective, spec.Objective) {
			return ErrInvalidSpec
		}
		if c.Budget.TokenOnly != spec.Budget.TokenOnly || c.Budget.PricingRevision != spec.Budget.PricingRevision || c.Budget.MaxTokens > spec.Budget.MaxTokens || c.Budget.MaxUSDMicros > spec.Budget.MaxUSDMicros || c.Limits.ExperimentTimeout > spec.Limits.ExperimentTimeout {
			return ErrInvalidSpec
		}
		for _, task := range c.Tasks {
			original, exists := registered[task.Digest]
			if !exists || !equalImprovementTemplate(original, task) {
				return ErrInvalidSpec
			}
			if sequence, exists := positions[task.InputDigest]; exists && sequence != p.Sequence {
				return ErrInvalidSpec
			}
			positions[task.InputDigest] = p.Sequence
			covered[task.Digest] = true
		}
		childPlan, err := Preflight(c)
		if err != nil {
			return err
		}
		if planErr := child.EvaluationPlan.Validate(c); planErr != nil {
			return planErr
		}
		if childPlan.TotalTrials > spec.Limits.MaxTrials-total {
			return ErrBudgetExceeded
		}
		total += childPlan.TotalTrials
		if i%2 == 1 {
			previous := plan.Children[i-1]
			left, right := previous.Spec, c
			left.Parent, right.Parent = nil, nil
			right.Controller = left.Controller
			if !equalImprovementTemplate(left, right) || !equalImprovementTemplate(previous.EvaluationPlan, child.EvaluationPlan) {
				return ErrInvalidSpec
			}
		}
	}
	if len(covered) != len(spec.Tasks) {
		return ErrInvalidSpec
	}
	return nil
}

func equalImprovementTemplate(left, right any) bool {
	a, err := canonical.Marshal(left)
	if err != nil {
		return false
	}
	b, err := canonical.Marshal(right)
	return err == nil && bytes.Equal(a, b)
}
