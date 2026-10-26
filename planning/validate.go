package planning

import (
	"fmt"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func validateFields(p Plan, limits Limits) error {
	limits, err := normalizeLimits(limits)
	if err != nil {
		return err
	}
	if (p.SchemaVersion != 1 && p.SchemaVersion != 2) || p.ID.IsNil() || p.ID.Prefix() != PrefixPlan || p.Revision < 1 || !textValid(p.Objective, p.Actor, p.Reason, p.Scope.Canonical()) {
		return invalid("identity or audit metadata")
	}
	if err = cortex.ValidateScope(p.Scope); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if p.Revision == 1 && (p.PreviousDigest != "" || len(p.AffectedTasks) != 0) || p.Revision > 1 && !evidence.ValidDigest(p.PreviousDigest) {
		return invalid("revision chain")
	}
	if err = p.Contract.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if p.Contract.Scope.Canonical() != p.Scope.Canonical() || p.Objective != p.Contract.Objective {
		return invalid("contract binding")
	}
	if p.Origin != (Origin{}) && (p.Origin.Kind != "orchestration" || p.Origin.ID.IsNil() || p.Origin.ID.Prefix() != id.PrefixOrchestrationConfig || !evidence.ValidDigest(p.Origin.Digest)) {
		return invalid("origin reference")
	}
	if !boundedBudget(p.Budget) {
		return invalid("finite plan budget required")
	}
	if len(p.Tasks) == 0 || len(p.Tasks) > limits.MaxTasks || len(p.Owners) == 0 || len(p.Owners) > 256 {
		return invalid("graph or owner count")
	}
	owners := make(map[string]bool, len(p.Owners))
	for _, owner := range p.Owners {
		if !textValid(owner) || owners[owner] {
			return invalid("owner roster")
		}
		owners[owner] = true
	}
	if resourceErr := validateResources(p.Resources); resourceErr != nil {
		return resourceErr
	}
	criteria := make(map[string]bool, len(p.Contract.Criteria))
	covered := make(map[string]bool, len(p.Contract.Criteria))
	for _, criterion := range p.Contract.Criteria {
		criteria[criterion.ID] = true
	}
	tasks := make(map[id.ID]Task, len(p.Tasks))
	artifacts := make(map[string]bool, len(p.Tasks))
	edges := 0
	hasReview := false
	for _, task := range p.Tasks {
		if task.ID.IsNil() || task.ID.Prefix() != PrefixTask || !textValid(task.Instruction, task.Owner) || !owners[task.Owner] {
			return invalid("task identity or owner")
		}
		hasReview = hasReview || task.Review != nil
		if task.Review != nil && (p.SchemaVersion != 2 || !textValid(task.Review.ReviewerAgent, task.Review.ReviewerPrincipal, task.Review.PolicyRevision) || len(task.Review.ReviewerAgent) > 256 || len(task.Review.ReviewerPrincipal) > 512 || len(task.Review.PolicyRevision) > 256) {
			return invalid("review requirement")
		}
		if _, exists := tasks[task.ID]; exists {
			return invalid("duplicate task")
		}
		if !budgetWithin(task.Budget, p.Budget) {
			return invalid("task budget exceeds plan")
		}
		if resourceErr := validateResources(task.Resources); resourceErr != nil {
			return resourceErr
		}
		if !resourcesWithin(task.Resources, p.Resources) {
			return invalid("task resources exceed plan")
		}
		if !textValid(task.Output.Name, task.Output.MediaType) || task.Output.MaxBytes <= 0 || artifacts[task.Output.Name] {
			return invalid("artifact ownership or bound")
		}
		artifacts[task.Output.Name] = true
		if len(task.Criteria) == 0 || len(task.Criteria) > len(criteria) {
			return invalid("task criteria")
		}
		seen := make(map[string]bool, len(task.Criteria))
		for _, criterion := range task.Criteria {
			if !criteria[criterion] || seen[criterion] {
				return invalid("unknown or repeated criterion")
			}
			seen[criterion] = true
			covered[criterion] = true
		}
		if len(task.DependsOn) > limits.MaxTasks {
			return invalid("dependency count")
		}
		edges += len(task.DependsOn)
		if edges > limits.MaxEdges {
			return invalid("edge count")
		}
		tasks[task.ID] = task
	}
	if p.SchemaVersion == 2 && !hasReview {
		return invalid("review schema requires a review policy")
	}
	for _, criterion := range p.Contract.Criteria {
		if criterion.Required && !covered[criterion.ID] {
			return invalid("required criterion has no task")
		}
	}
	seenAffected := make(map[id.ID]bool, len(p.AffectedTasks))
	if len(p.AffectedTasks) > 512 {
		return invalid("affected task count")
	}
	// Deleted task IDs remain in the audit set, so they need not be in this graph.
	for _, taskID := range p.AffectedTasks {
		if taskID.IsNil() || taskID.Prefix() != PrefixTask || seenAffected[taskID] {
			return invalid("affected task identity")
		}
		seenAffected[taskID] = true
	}
	if err := validateRequirements(p); err != nil {
		return err
	}
	return validateDAG(tasks, limits.MaxDepth)
}

func normalizeLimits(l Limits) (Limits, error) {
	bounds := []struct {
		value   *int
		ceiling int
	}{{&l.MaxTasks, 256}, {&l.MaxDepth, 64}, {&l.MaxEdges, 4096}}
	for _, bound := range bounds {
		if *bound.value < 0 || *bound.value > bound.ceiling {
			return Limits{}, invalid("graph limits")
		}
		if *bound.value == 0 {
			*bound.value = bound.ceiling
		}
	}
	return l, nil
}

func boundedBudget(b execution.Budget) bool {
	return b.Validate() == nil && b.MaxSteps > 0 && b.MaxTokens > 0 && b.MaxToolCalls > 0 && !b.Deadline.IsZero()
}

func budgetWithin(child, parent execution.Budget) bool {
	return boundedBudget(child) && child.MaxSteps <= parent.MaxSteps && child.MaxTokens <= parent.MaxTokens && child.MaxToolCalls <= parent.MaxToolCalls && !child.Deadline.After(parent.Deadline)
}

func validateResources(resources []ResourceGrant) error {
	if len(resources) > 256 {
		return invalid("resource count")
	}
	seen := make(map[string]bool, len(resources))
	for _, resource := range resources {
		if !textValid(resource.Resource, resource.Revision) || seen[resource.Resource] || (resource.Access != AccessRead && resource.Access != AccessWrite) {
			return invalid("resource grant")
		}
		seen[resource.Resource] = true
	}
	return nil
}

func resourcesWithin(child, parent []ResourceGrant) bool {
	allowed := make(map[string]ResourceGrant, len(parent))
	for _, grant := range parent {
		allowed[grant.Resource] = grant
	}
	for _, grant := range child {
		ceiling, ok := allowed[grant.Resource]
		if !ok || grant.Revision != ceiling.Revision || grant.Access == AccessWrite && ceiling.Access != AccessWrite {
			return false
		}
	}
	return true
}

func validateDAG(tasks map[id.ID]Task, maxDepth int) error {
	visiting := make(map[id.ID]bool, len(tasks))
	depths := make(map[id.ID]int, len(tasks))
	var visit func(id.ID) (int, error)
	visit = func(taskID id.ID) (int, error) {
		if visiting[taskID] {
			return 0, invalid("dependency cycle")
		}
		if depth := depths[taskID]; depth > 0 {
			return depth, nil
		}
		task, ok := tasks[taskID]
		if !ok {
			return 0, invalid("unknown dependency")
		}
		visiting[taskID] = true
		depth := 1
		seen := make(map[id.ID]bool, len(task.DependsOn))
		for _, dependency := range task.DependsOn {
			if seen[dependency] {
				return 0, invalid("duplicate dependency")
			}
			seen[dependency] = true
			upstream, err := visit(dependency)
			if err != nil {
				return 0, err
			}
			depth = max(depth, upstream+1)
			if depth > maxDepth {
				return 0, invalid("graph depth")
			}
		}
		visiting[taskID] = false
		depths[taskID] = depth
		return depth, nil
	}
	for taskID := range tasks {
		if _, err := visit(taskID); err != nil {
			return err
		}
	}
	return nil
}
