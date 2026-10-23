package planning

import (
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// TaskContract derives a task's checks using a new host-selected contract ID.
// Persist the returned contract before executing or verifying that task. It
// preserves plan constraints and verifier parameters, and requires every assigned
// criterion even when that criterion is optional for the plan as a whole.
func TaskContract(p Plan, taskID, contractID id.ID) (verification.Contract, error) {
	if Validate(p, Limits{}) != nil {
		return verification.Contract{}, ErrInvalid
	}
	for _, task := range p.Tasks {
		if task.ID == taskID {
			return taskContract(p, task, contractID)
		}
	}
	return verification.Contract{}, ErrInvalid
}

func taskContract(p Plan, task Task, contractID id.ID) (verification.Contract, error) {
	if contractID.IsNil() || contractID.Prefix() != verification.PrefixContract || contractID == p.Contract.ID {
		return verification.Contract{}, ErrInvalid
	}
	assigned := make(map[string]bool, len(task.Criteria))
	for _, criterion := range task.Criteria {
		assigned[criterion] = true
	}
	criteria := make([]verification.Criterion, 0, len(task.Criteria))
	// Keep contract ordering. Task definitions cannot reorder verifier policy.
	for _, criterion := range p.Contract.Criteria {
		if assigned[criterion.ID] {
			criterion.Required = true
			criteria = append(criteria, criterion)
		}
	}
	return verification.SealContract(verification.Contract{SchemaVersion: 1, ID: contractID, Scope: p.Scope, Revision: "task/" + ArtifactProducer(p, task.ID), Objective: task.Instruction, Constraints: p.Contract.Constraints, Criteria: criteria})
}

// ValidateTaskContract accepts the exact plan contract or its task projection.
// It rejects omitted, weakened, foreign or superseded task requirements. Contract
// integrity does not establish current host authority or verifier authenticity.
func ValidateTaskContract(p Plan, taskID id.ID, contract verification.Contract) error {
	if Validate(p, Limits{}) != nil || contract.Validate() != nil {
		return ErrInvalid
	}
	for _, task := range p.Tasks {
		if task.ID == taskID {
			return validateTaskContract(p, task, contract)
		}
	}
	return ErrInvalid
}

func validateTaskContract(p Plan, task Task, contract verification.Contract) error {
	if contract.Digest == p.Contract.Digest {
		return nil
	}
	expected, err := taskContract(p, task, contract.ID)
	if err != nil || expected.Digest != contract.Digest {
		return ErrInvalid
	}
	return nil
}
