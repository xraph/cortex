package engine

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

type reflectionOwnershipStore struct {
	store.Store
	coordination.ClaimStore
	coordination.BudgetedClaimStore
	claim coordination.Claim
}

func (s *reflectionOwnershipStore) GetBudgetedTaskClaim(context.Context, id.ID) (coordination.BudgetedClaim, error) {
	return coordination.BudgetedClaim{Claim: s.claim}, nil
}
func (s *reflectionOwnershipStore) GetTaskClaim(context.Context, id.ID, id.ID) (coordination.Claim, error) {
	return s.claim, nil
}
func TestReflectionRejectsHistoricalTaskOwnership(t *testing.T) {
	for _, state := range []coordination.ClaimState{coordination.ClaimDispatching, coordination.ClaimRecovering, coordination.ClaimRetired} {
		t.Run(string(state), func(t *testing.T) {
			now := time.Now().UTC()
			scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "reflection"}}}
			runID := id.NewAgentRunID()
			claim := coordination.Claim{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), Scope: scope, PlanID: id.New(planning.PrefixPlan), PlanRevision: 1, PlanDigest: evidence.Digest([]byte("plan")), TaskID: id.New(planning.PrefixTask), Worker: coordination.Identity{Agent: "worker", Principal: "host"}, GrantRef: "grant", Fence: 1, Revision: 2, State: state, RunID: runID, AcquiredAt: now.Add(-time.Minute), At: now.Add(-30 * time.Second), ExpiresAt: now.Add(-time.Second), Deadline: now.Add(time.Minute), PreviousDigest: evidence.Digest([]byte("previous"))}
			if state != coordination.ClaimDispatching {
				claim.At = now
			}
			if state == coordination.ClaimRetired {
				claim.SchemaVersion = 2
				claim.RetirementDigest = evidence.Digest([]byte("retired"))
			}
			bytes, err := json.Marshal(claim)
			require.NoError(t, err)
			claim.Digest = evidence.Digest(bytes)
			require.NoError(t, claim.Validate())
			e := &Engine{store: &reflectionOwnershipStore{claim: claim}}
			_, err = e.reflectionSharedPlan(cortex.WithScope(context.Background(), scope), &run.Run{ID: runID, Scope: scope, Provenance: run.ExecutionProvenance{TaskClaimID: claim.ID.String()}})
			require.ErrorIs(t, err, cognitive.ErrReflectionConflict)
		})
	}
}
