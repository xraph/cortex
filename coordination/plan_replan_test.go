package coordination_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

func replanRequestFixture(t *testing.T) coordination.PlanReplanRequest {
	_, p, _, now := handoffFixture(t)
	attempt := id.New(coordination.PrefixPlanReplan)
	progress := planning.ProgressSnapshot{SchemaVersion: 1, Plan: p, At: now, StartedAt: now.Add(-time.Hour), LastProgressAt: now.Add(-time.Hour), Account: execution.BudgetSnapshot{Policy: execution.ReservationPolicy{SchemaVersion: 1, PlanID: p.ID, Scope: p.Scope, Limits: execution.Resources{Steps: int64(p.Budget.MaxSteps), Tokens: int64(p.Budget.MaxTokens), ToolCalls: int64(p.Budget.MaxToolCalls), FanOut: 10, Replans: 2}, Deadline: p.Budget.Deadline, TokenOnly: true}}}
	return coordination.PlanReplanRequest{SchemaVersion: 1, ID: attempt, Progress: progress, Policy: planning.ReplanPolicy{}, Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: p.Tasks[0].ID, OwnerRef: attempt.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 1, Tokens: 10, FanOut: 1, Replans: 1}, Deadline: p.Budget.Deadline}, RunID: id.NewAgentRunID(), SessionID: id.NewSessionID(), AgentID: id.NewAgentID(), PreparedDigest: evidence.Digest([]byte("prepared")), InputDigest: evidence.Digest([]byte("input")), Actor: "host", Reason: "bounded replacement"}
}

func TestPlanReplanAdmissionFreezesBindingsAndRequiresAttemptCharge(t *testing.T) {
	req := replanRequestFixture(t)
	frozen, digest, err := req.Freeze()
	require.NoError(t, err)
	require.True(t, evidence.ValidDigest(digest))
	req.Progress.Plan.Tasks[0].Instruction = "mutated"
	require.NotEqual(t, req.Progress.Plan.Tasks[0].Instruction, frozen.Progress.Plan.Tasks[0].Instruction)
	for _, mode := range []string{"replan", "tools", "owner", "plan", "prepared", "future"} {
		t.Run(mode, func(t *testing.T) {
			changed := replanRequestFixture(t)
			switch mode {
			case "replan":
				changed.Reservation.Allocation.Replans = 0
			case "tools":
				changed.Reservation.Allocation.ToolCalls = 1
			case "owner":
				changed.Reservation.OwnerRef = "different"
			case "plan":
				changed.Reservation.PlanDigest = evidence.Digest(nil)
			case "prepared":
				changed.PreparedDigest = ""
			case "future":
				changed.Progress.At = changed.Progress.Plan.Budget.Deadline.Add(time.Second)
			}
			_, _, err := changed.Freeze()
			require.Error(t, err)
		})
	}
}
