package planning

import (
	"bytes"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// Status describes scheduler state. Succeeded alone never proves acceptance.
type Status string

const (
	StatusPending   Status = "pending"
	StatusReady     Status = "ready"
	StatusClaimed   Status = "claimed"
	StatusRunning   Status = "running"
	StatusWaiting   Status = "waiting"
	StatusVerifying Status = "verifying"
	StatusSucceeded Status = "succeeded"
	StatusFailed    Status = "failed"
	StatusBlocked   Status = "blocked"
	StatusCancelled Status = "cancelled"
)

// TaskState is a host-supplied readiness snapshot. Current must be rebuilt from
// currently authorized artifact and environment revisions, not copied from a
// persisted report. Reports must come from trusted verifier storage. This type
// carries no credentials and cannot authenticate remote success claims.
type TaskState struct {
	TaskID     id.ID                 `json:"task_id"`
	PlanDigest string                `json:"plan_digest"`
	Status     Status                `json:"status"`
	Current    verification.Binding  `json:"current"`
	Reports    []verification.Report `json:"reports,omitempty"`
}

// Ready returns pending tasks in definition order whose entire prerequisite
// chain has current verified artifacts. It performs no I/O or authorization.
// The scheduler must recheck grants and atomically claim work before dispatch.
// Any revision requires explicitly refreshed states; evidence is never reused
// solely because a task ID survived the revision.
func Ready(p Plan, states []TaskState, at time.Time) ([]id.ID, error) {
	if err := Validate(p, Limits{}); err != nil {
		return nil, err
	}
	if at.IsZero() || len(states) > len(p.Tasks) {
		return nil, invalid("readiness snapshot")
	}
	definitions := make(map[id.ID]Task, len(p.Tasks))
	for _, task := range p.Tasks {
		definitions[task.ID] = task
	}
	snapshots := make(map[id.ID]TaskState, len(states))
	for _, state := range states {
		_, known := definitions[state.TaskID]
		_, duplicate := snapshots[state.TaskID]
		if !known || duplicate || state.PlanDigest != p.Digest || !validStatus(state.Status) {
			return nil, invalid("task state binding")
		}
		snapshots[state.TaskID] = state
	}
	if !at.Before(p.Budget.Deadline) {
		return nil, nil
	}
	accepted := make(map[id.ID]bool, len(states))
	checked := make(map[id.ID]bool, len(states))
	var accepts func(id.ID) bool
	accepts = func(taskID id.ID) bool {
		if checked[taskID] {
			return accepted[taskID]
		}
		checked[taskID] = true
		task := definitions[taskID]
		state, ok := snapshots[taskID]
		if !ok || state.Status != StatusSucceeded || !taskAccepted(p, task, state, at) {
			return false
		}
		for _, dependency := range task.DependsOn {
			if !accepts(dependency) || !containsRef(state.Current.Evidence, snapshots[dependency].Current.Candidate) {
				return false
			}
		}
		accepted[taskID] = true
		return true
	}
	var ready []id.ID
	for _, task := range p.Tasks {
		state, exists := snapshots[task.ID]
		if exists && state.Status != StatusPending && state.Status != StatusReady {
			continue
		}
		if !at.Before(task.Budget.Deadline) {
			continue
		}
		available := true
		for _, dependency := range task.DependsOn {
			if !accepts(dependency) {
				available = false
				break
			}
		}
		if available {
			ready = append(ready, task.ID)
		}
	}
	return ready, nil
}

func taskAccepted(p Plan, task Task, state TaskState, at time.Time) bool {
	binding := state.Current
	if !taskBindingValid(p, task, binding) {
		return false
	}
	if len(state.Reports) == 0 || len(state.Reports) > 256 {
		return false
	}
	for _, report := range state.Reports {
		if verification.CheckFreshness(report, binding, at) != verification.StatusPassed {
			return false
		}
	}
	verdict := verification.Aggregate(p.Contract, state.Reports, at)
	checks := make(map[string]verification.Status, len(verdict.Criteria))
	for _, criterion := range verdict.Criteria {
		checks[criterion.CriterionID] = criterion.Status
	}
	// Each assigned check is required for this task, even if it is optional for
	// the plan as a whole. Aggregate has already rejected conflicting reports.
	for _, criterion := range task.Criteria {
		if checks[criterion] != verification.StatusPassed {
			return false
		}
	}
	return true
}

func containsRef(refs []evidence.Ref, want evidence.Ref) bool {
	expected, err := encode(want)
	if err != nil {
		return false
	}
	for _, ref := range refs {
		if ref.ID != want.ID {
			continue
		}
		actual, encodeErr := encode(ref)
		return encodeErr == nil && bytes.Equal(actual, expected)
	}
	return false
}

func validStatus(status Status) bool {
	switch status {
	case StatusPending, StatusReady, StatusClaimed, StatusRunning, StatusWaiting, StatusVerifying, StatusSucceeded, StatusFailed, StatusBlocked, StatusCancelled:
		return true
	default:
		return false
	}
}

// ArtifactProducer identifies output captured for a task in this exact sealed
// plan. Use it when the host captures an artifact before trusted verification.
// A new plan revision requires new verification even when bytes are unchanged.
func ArtifactProducer(p Plan, taskID id.ID) string {
	return p.Digest + ":" + taskID.String()
}

// ValidateTaskBinding checks candidate shape and frozen task sources/tools. It
// does not establish provenance authenticity, current access or a verifier pass.
func ValidateTaskBinding(p Plan, taskID id.ID, binding verification.Binding) error {
	if Validate(p, Limits{}) != nil {
		return ErrInvalid
	}
	for _, task := range p.Tasks {
		if task.ID == taskID && taskBindingValid(p, task, binding) {
			return nil
		}
	}
	return ErrInvalid
}

func taskBindingValid(p Plan, task Task, binding verification.Binding) bool {
	return binding.Validate() == nil && binding.Scope.Canonical() == p.Scope.Canonical() && binding.ContractDigest == p.Contract.Digest && taskContextMatches(p, task, binding.Context) && binding.Candidate.Producer == ArtifactProducer(p, task.ID) && binding.Candidate.MediaType == task.Output.MediaType && binding.Candidate.Bytes <= task.Output.MaxBytes
}
