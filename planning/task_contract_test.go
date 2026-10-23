package planning

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func taskContractPlan(t *testing.T) Plan {
	t.Helper()
	p := fixturePlan(t)
	second := p.Contract.Criteria[0]
	second.ID, second.Required = "second", false
	p.Contract.Criteria = append(p.Contract.Criteria, second)
	p.Contract.Constraints = []string{"retain evidence provenance"}
	var err error
	p.Contract, err = verification.SealContract(p.Contract)
	require.NoError(t, err)
	child := p.Tasks[0]
	child.ID, child.Instruction, child.Output.Name = id.New(PrefixTask), "check second output", "second-output"
	child.Criteria, child.DependsOn = []string{"second"}, []id.ID{p.Tasks[0].ID}
	p.Tasks = append(p.Tasks, child)
	p, err = Seal(p, Limits{})
	require.NoError(t, err)
	return p
}

func TestTaskContractPreservesRequirements(t *testing.T) {
	p := taskContractPlan(t)
	task := p.Tasks[1]
	identifier := id.New(verification.PrefixContract)
	contract, err := TaskContract(p, task.ID, identifier)
	require.NoError(t, err)
	require.Equal(t, identifier, contract.ID)
	require.Equal(t, p.Scope, contract.Scope)
	require.Equal(t, task.Instruction, contract.Objective)
	require.Equal(t, p.Contract.Constraints, contract.Constraints)
	expected := p.Contract.Criteria[1]
	expected.Required = true
	require.Equal(t, []verification.Criterion{expected}, contract.Criteria, "every assigned check is required for its task")
	require.NoError(t, ValidateTaskContract(p, task.ID, contract))
	require.NoError(t, ValidateTaskContract(p, task.ID, p.Contract), "existing plan-bound runs remain valid")
	again, err := TaskContract(p, task.ID, identifier)
	require.NoError(t, err)
	require.Equal(t, contract, again)
	contract.Scope.Levels[0].Value = "changed"
	contract.Constraints[0] = "changed"
	contract.Criteria[0].Verifier = "changed"
	require.NotEqual(t, p.Scope, contract.Scope)
	require.Equal(t, "retain evidence provenance", p.Contract.Constraints[0])
	require.NotEqual(t, "changed", p.Contract.Criteria[1].Verifier)
}

func TestTaskContractRejectsWeakenedOrForeignChecks(t *testing.T) {
	p := taskContractPlan(t)
	contract, err := TaskContract(p, p.Tasks[0].ID, id.New(verification.PrefixContract))
	require.NoError(t, err)
	for name, mutate := range map[string]func(*verification.Contract){
		"verifier":    func(c *verification.Contract) { c.Criteria[0].VerifierRevision = "other" },
		"manifest":    func(c *verification.Contract) { c.Criteria[0].Parameters.ManifestRevision = "other" },
		"freshness":   func(c *verification.Contract) { c.Criteria[0].MaxAge += time.Hour },
		"source_age":  func(c *verification.Contract) { c.Criteria[0].MaxSourceAge += time.Hour },
		"evidence":    func(c *verification.Contract) { c.Criteria[0].MinEvidence++ },
		"objective":   func(c *verification.Contract) { c.Objective = "different" },
		"constraints": func(c *verification.Contract) { c.Constraints = nil },
		"extra_check": func(c *verification.Contract) { c.Criteria = append(c.Criteria, p.Contract.Criteria[1]) },
		"revision":    func(c *verification.Contract) { c.Revision = "different" },
	} {
		t.Run(name, func(t *testing.T) {
			changed, freezeErr := verification.SealContract(contract)
			require.NoError(t, freezeErr)
			mutate(&changed)
			changed, freezeErr = verification.SealContract(changed)
			require.NoError(t, freezeErr)
			require.ErrorIs(t, ValidateTaskContract(p, p.Tasks[0].ID, changed), ErrInvalid)
		})
	}
	require.ErrorIs(t, ValidateTaskContract(p, p.Tasks[1].ID, contract), ErrInvalid)
	_, err = TaskContract(p, p.Tasks[0].ID, p.Contract.ID)
	require.ErrorIs(t, err, ErrInvalid, "projection must not reuse the plan contract ID")
	_, err = TaskContract(p, id.New(PrefixTask), id.New(verification.PrefixContract))
	require.ErrorIs(t, err, ErrInvalid)
	next, err := ApplyRevision(p, Revision{Plan: p, Actor: "host", Reason: "new revision"}, p.Revision, Limits{})
	require.NoError(t, err)
	require.ErrorIs(t, ValidateTaskContract(next, next.Tasks[0].ID, contract), ErrInvalid)
}

func TestReadyUsesExactTaskContract(t *testing.T) {
	for _, mode := range []string{"valid", "missing_contract", "different_task", "weakened", "failed", "stale", "new_plan"} {
		t.Run(mode, func(t *testing.T) {
			p := taskContractPlan(t)
			at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
			state := acceptedState(t, p, p.Tasks[0], at)
			contract, err := TaskContract(p, p.Tasks[0].ID, id.New(verification.PrefixContract))
			require.NoError(t, err)
			if mode == "different_task" {
				contract, err = TaskContract(p, p.Tasks[1].ID, id.New(verification.PrefixContract))
				require.NoError(t, err)
			}
			if mode == "weakened" {
				contract.Criteria[0].MaxAge += time.Hour
				contract, err = verification.SealContract(contract)
				require.NoError(t, err)
			}
			state.Contract = &contract
			state.Current.ContractDigest = contract.Digest
			state.Reports[0].Binding = state.Current
			if mode == "failed" {
				state.Reports[0].Outcomes[0].Status = verification.StatusFailed
			}
			state.Reports[0], err = verification.SealReport(state.Reports[0])
			require.NoError(t, err)
			if mode == "missing_contract" {
				state.Contract = nil
			}
			if mode == "stale" {
				at = at.Add(time.Minute)
			}
			if mode == "new_plan" {
				p, err = ApplyRevision(p, Revision{Plan: p, Actor: "host", Reason: "new"}, p.Revision, Limits{})
				require.NoError(t, err)
				state.PlanDigest = p.Digest
			}
			ready, err := Ready(p, []TaskState{state}, at)
			require.NoError(t, err)
			if mode == "valid" {
				require.Equal(t, []id.ID{p.Tasks[1].ID}, ready)
			} else {
				require.Empty(t, ready)
			}
		})
	}
}

func TestTaskContractRequiresEveryAssignedCheckInPlanOrder(t *testing.T) {
	p := taskContractPlan(t)
	p.Tasks[0].Criteria = []string{"second", "checked"}
	var err error
	p, err = Seal(p, Limits{})
	require.NoError(t, err)
	contract, err := TaskContract(p, p.Tasks[0].ID, id.New(verification.PrefixContract))
	require.NoError(t, err)
	require.Equal(t, "checked", contract.Criteria[0].ID)
	require.Equal(t, "second", contract.Criteria[1].ID)
	require.True(t, contract.Criteria[1].Required)
	at := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	state := acceptedState(t, p, p.Tasks[0], at)
	state.Contract = &contract
	state.Current.ContractDigest = contract.Digest
	state.Reports[0].Binding = state.Current
	state.Reports[0], err = verification.SealReport(state.Reports[0])
	require.NoError(t, err)
	ready, err := Ready(p, []TaskState{state}, at)
	require.NoError(t, err)
	require.Empty(t, ready, "an optional plan check remains required when assigned to this task")
	outcome := state.Reports[0].Outcomes[0]
	outcome.CriterionID = "second"
	state.Reports[0].Outcomes = append(state.Reports[0].Outcomes, outcome)
	state.Reports[0], err = verification.SealReport(state.Reports[0])
	require.NoError(t, err)
	ready, err = Ready(p, []TaskState{state}, at)
	require.NoError(t, err)
	require.Equal(t, []id.ID{p.Tasks[1].ID}, ready)
	weakened, err := verification.SealContract(contract)
	require.NoError(t, err)
	weakened.Criteria[1].Required = false
	weakened, err = verification.SealContract(weakened)
	require.NoError(t, err)
	require.ErrorIs(t, ValidateTaskContract(p, p.Tasks[0].ID, weakened), ErrInvalid)
}
