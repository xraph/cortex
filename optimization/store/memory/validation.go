package memory

import (
	"context"
	"slices"
	"sort"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

// GetValidationAllocation returns the frozen roster and its exposure history.
func (s *Store) GetValidationAllocation(ctx context.Context, allocationID id.ID) (optimization.ValidationAllocation, error) {
	var empty optimization.ValidationAllocation
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	if data := s.scopes[key]; data != nil {
		if allocation, exists := data.Allocations[allocationID.String()]; exists {
			return clone(allocation)
		}
	}
	return empty, optimization.ErrNotFound
}

func (tx *transaction) AllocateValidation(request optimization.ValidationAllocationRequest) (optimization.ValidationAllocation, error) {
	var empty optimization.ValidationAllocation
	if !tx.active {
		return empty, optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return empty, err
	}
	if _, err := optimization.ParseID(request.ID.String(), optimization.PrefixValidation); err != nil {
		return empty, err
	}
	owner, exists := tx.data.Experiments[request.ExperimentID.String()]
	if !exists {
		return empty, optimization.ErrNotFound
	}
	if !tx.validLease(owner, request.Lease) || owner.Phase != optimization.PhaseValidate {
		return empty, optimization.ErrConflict
	}
	if previous, found := tx.data.Allocations[request.ID.String()]; found {
		if previous.ExperimentID != request.ExperimentID || !previous.ReleasedAt.IsZero() || !same(previous.PairedRoster, request.PairedRoster) {
			return empty, optimization.ErrConflict
		}
		return clone(previous)
	}
	tasks := validationTasks(owner.Spec)
	if len(tasks) == 0 || len(request.PairedRoster) < 1 || len(request.PairedRoster) > 2 {
		return empty, optimization.ErrInvalidSpec
	}
	if len(request.PairedRoster) == 1 && request.PairedRoster[0].ExperimentID != owner.ID {
		return empty, optimization.ErrInvalidSpec
	}
	seen := make(map[string]bool)
	for _, arm := range request.PairedRoster {
		if seen[arm.ExperimentID.String()] || (len(request.PairedRoster) == 2 && arm.ExperimentID == owner.ID) {
			return empty, optimization.ErrInvalidSpec
		}
		seen[arm.ExperimentID.String()] = true
		child, found := tx.data.Experiments[arm.ExperimentID.String()]
		if !found {
			return empty, optimization.ErrNotFound
		}
		if child.Spec.AgentID != owner.Spec.AgentID || !same(validationTasks(child.Spec), tasks) || child.Spec.Controller != arm.Controller || child.Spec.Baseline != arm.Baseline {
			return empty, optimization.ErrPolicyDenied
		}
		if child.Spec.Evaluation != owner.Spec.Evaluation || !same(child.Spec.Objective, owner.Spec.Objective) || child.Spec.Limits.Repetitions != owner.Spec.Limits.Repetitions {
			return empty, optimization.ErrPolicyDenied
		}
		if child.State != optimization.StateRunning || child.Phase != optimization.PhaseValidate {
			return empty, optimization.ErrConflict
		}
		if !tx.executionVersion(arm.Finalist, owner.Spec.AgentID) {
			return empty, optimization.ErrStaleVersion
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
				return empty, optimization.ErrNotFound
			}
		}
	}
	allocation := optimization.ValidationAllocation{CreatedAt: tx.now, ID: request.ID, Scope: owner.Spec.Scope, AgentID: owner.Spec.AgentID, ExperimentID: owner.ID, PairedRoster: request.PairedRoster, Revision: 1, CaseDigests: []string{}, InputDigests: []string{}, DatasetItems: []optimization.ContentRef{}}
	for _, task := range tasks {
		allocation.CaseDigests = append(allocation.CaseDigests, task.Digest)
		allocation.InputDigests = append(allocation.InputDigests, task.InputDigest)
		if task.DatasetItem != nil {
			allocation.DatasetItems = append(allocation.DatasetItems, *task.DatasetItem)
		}
	}
	for _, previous := range tx.data.Allocations {
		if previous.AgentID != allocation.AgentID || !previous.ReleasedAt.IsZero() {
			continue
		}
		if overlaps(previous.InputDigests, allocation.InputDigests) || overlaps(previous.CaseDigests, allocation.CaseDigests) || overlappingItems(previous.DatasetItems, allocation.DatasetItems) {
			return empty, optimization.ErrConflict
		}
	}
	detached, err := clone(allocation)
	if err != nil {
		return empty, err
	}
	tx.data.Allocations[allocation.ID.String()] = detached
	tx.validationEvent(owner, allocation, "validation_allocated", request.Lease.Owner)
	return clone(detached)
}

func (tx *transaction) ExposeValidation(access optimization.ValidationAccess) error {
	return tx.updateValidation(access, false)
}

func (tx *transaction) ReleaseValidation(access optimization.ValidationAccess) error {
	return tx.updateValidation(access, true)
}

func (tx *transaction) updateValidation(access optimization.ValidationAccess, release bool) error {
	if !tx.active {
		return optimization.ErrConflict
	}
	if err := tx.ctx.Err(); err != nil {
		return err
	}
	allocation, exists := tx.data.Allocations[access.AllocationID.String()]
	if !exists {
		return optimization.ErrNotFound
	}
	experiment, exists := tx.data.Experiments[access.ExperimentID.String()]
	if !exists {
		return optimization.ErrNotFound
	}
	if !tx.validLease(experiment, access.Lease) || experiment.Phase != optimization.PhaseValidate {
		return optimization.ErrConflict
	}
	if release {
		if experiment.ID != allocation.ExperimentID {
			return optimization.ErrPolicyDenied
		}
		if !allocation.ExposedAt.IsZero() {
			return optimization.ErrConflict
		}
		if !allocation.ReleasedAt.IsZero() {
			return nil
		}
		allocation.ReleasedAt = tx.now
	} else {
		if !slices.ContainsFunc(allocation.PairedRoster, func(arm optimization.ValidationArm) bool { return arm.ExperimentID == access.ExperimentID }) {
			return optimization.ErrPolicyDenied
		}
		if !allocation.ReleasedAt.IsZero() {
			return optimization.ErrConflict
		}
		if !allocation.ExposedAt.IsZero() {
			return nil
		}
		allocation.ExposedAt = tx.now
	}
	allocation.Revision++
	tx.data.Allocations[allocation.ID.String()] = allocation
	eventType := "validation_exposed"
	if release {
		eventType = "validation_released"
	}
	owner := tx.data.Experiments[allocation.ExperimentID.String()]
	tx.validationEvent(owner, allocation, eventType, access.Lease.Owner)
	return nil
}

func (tx *transaction) validationEvent(owner optimization.Experiment, allocation optimization.ValidationAllocation, eventType, actor string) {
	event := tx.appendEvent(owner, eventType, optimization.IdentityRef{Subject: actor})
	event.Resources = append(event.Resources, allocation.ID)
	for _, arm := range allocation.PairedRoster {
		event.Resources = append(event.Resources, arm.ExperimentID, arm.Controller.ID, arm.Baseline.ID, arm.Finalist.ID)
	}
}

func (tx *transaction) executionVersion(ref optimization.VersionRef, agentID id.AgentID) bool {
	version, exists := tx.data.Versions[ref.ID.String()]
	return exists && version.Digest == ref.Digest && version.AgentID == agentID && version.Kind == optimization.HarnessExecution
}

func validationTasks(spec optimization.ExperimentSpec) []optimization.TaskCase {
	tasks := []optimization.TaskCase{}
	for _, task := range spec.Tasks {
		if task.Split == optimization.SplitValidation {
			tasks = append(tasks, task)
		}
	}
	sort.Slice(tasks, func(i, j int) bool { return tasks[i].Digest < tasks[j].Digest })
	return tasks
}

func overlaps(a, b []string) bool {
	seen := make(map[string]bool, len(a))
	for _, value := range a {
		seen[value] = true
	}
	for _, value := range b {
		if seen[value] {
			return true
		}
	}
	return false
}

func overlappingItems(a, b []optimization.ContentRef) bool {
	for _, left := range a {
		for _, right := range b {
			if left.Ref == right.Ref && left.Revision == right.Revision {
				return true
			}
		}
	}
	return false
}
