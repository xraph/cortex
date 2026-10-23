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

func TestBudgetedClaimRequestBindsOneAttempt(t *testing.T) {
	h, p, _, _ := handoffFixture(t)
	claim := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}
	r := coordination.BudgetedClaimRequest{Claim: claim, Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: h.TaskID, OwnerRef: claim.ID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{FanOut: 1}, Deadline: p.Budget.Deadline}}
	frozen, digest, err := r.Freeze()
	require.NoError(t, err)
	require.True(t, evidence.ValidDigest(digest))
	for name, mutate := range map[string]func(*coordination.BudgetedClaimRequest){
		"plan": func(req *coordination.BudgetedClaimRequest) { req.Reservation.PlanID = id.New(planning.PrefixPlan) },
		"revision": func(req *coordination.BudgetedClaimRequest) {
			req.Reservation.PlanDigest = evidence.Digest([]byte("another"))
		},
		"task":  func(req *coordination.BudgetedClaimRequest) { req.Reservation.TaskID = id.New(planning.PrefixTask) },
		"owner": func(req *coordination.BudgetedClaimRequest) { req.Reservation.OwnerRef = "unrelated" },
		"branch": func(req *coordination.BudgetedClaimRequest) {
			req.Reservation.Kind = execution.ReservationBranch
			req.Reservation.Allocation.FanOut = 2
		},
		"invalid_claim":       func(req *coordination.BudgetedClaimRequest) { req.Claim.Duration = 0 },
		"invalid_reservation": func(req *coordination.BudgetedClaimRequest) { req.Reservation.Allocation.Tokens = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			changed := r
			mutate(&changed)
			_, _, err := changed.Freeze()
			require.Error(t, err)
		})
	}
	r.Reservation.Scope.Levels[0].Value = "mutated"
	require.NotEqual(t, r.Reservation.Scope, frozen.Reservation.Scope)
}
