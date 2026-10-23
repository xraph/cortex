package coordination_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func TestReadyTaskClaimRequestBindsDependencies(t *testing.T) {
	a := taskAcceptanceFixture(t)
	h, p, _, _ := handoffFixture(t)
	claim := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}
	a.Request.Current.Scope = p.Scope
	req := coordination.ReadyTaskClaimRequest{SchemaVersion: 1, Acquisition: coordination.BudgetedClaimRequest{Claim: claim, Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: h.TaskID, OwnerRef: claim.ID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{FanOut: 1}, Deadline: p.Budget.Deadline}}, Dependencies: []coordination.AcceptedDependency{{AcceptanceID: a.Request.ID, Digest: a.Digest, Current: a.Request.Current}}}
	frozen, digest, err := req.Freeze()
	require.NoError(t, err)
	require.NotEmpty(t, digest)
	for _, mode := range []string{"version", "duplicate", "digest", "scope", "allocation"} {
		t.Run(mode, func(t *testing.T) {
			changed, _, err := req.Freeze()
			require.NoError(t, err)
			switch mode {
			case "version":
				changed.SchemaVersion = 2
			case "duplicate":
				changed.Dependencies = append(changed.Dependencies, changed.Dependencies[0])
			case "digest":
				changed.Dependencies[0].Digest = "bad"
			case "scope":
				changed.Dependencies[0].Current.Scope.Levels[0].Value = "other"
			case "allocation":
				changed.Acquisition.Reservation.OwnerRef = "other"
			}
			_, _, err = changed.Freeze()
			require.Error(t, err)
		})
	}
	req.Dependencies[0].Current.Scope.Levels[0].Value = "mutated"
	require.NotEqual(t, req.Dependencies[0].Current.Scope, frozen.Dependencies[0].Current.Scope)
}
