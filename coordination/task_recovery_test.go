package coordination_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
)

func recoveringClaimFixture(t *testing.T) (coordination.Claim, coordination.TaskRecoveryProof) {
	t.Helper()
	h, p, _, now := handoffFixture(t)
	held, err := coordination.AcquireClaim(nil, coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}, p, now)
	require.NoError(t, err)
	dispatched, err := coordination.BeginDispatch(held, held.Token(), id.NewAgentRunID(), now)
	require.NoError(t, err)
	recovering, err := coordination.ExpireClaim(dispatched, dispatched.Revision, dispatched.ExpiresAt)
	require.NoError(t, err)
	req := execution.RunCallSealRequest{SchemaVersion: 1, Scope: recovering.Scope, RunID: recovering.RunID, Actor: "recovery", Reason: "freeze original invocation"}
	_, digest, err := req.Freeze()
	require.NoError(t, err)
	proof := coordination.TaskRecoveryProof{SchemaVersion: 1, ClaimDigest: recovering.Digest, Seal: execution.RunCallSeal{Request: req, RequestDigest: digest, At: recovering.At}, SnapshotDigest: evidence.Digest([]byte("complete snapshot")), Issuer: "fixture", Revision: "1", ProofRef: "stopped-incarnation", ProofDigest: evidence.Digest([]byte("quiescence proof")), ObservedAt: recovering.At, ExpiresAt: recovering.At.Add(time.Minute)}
	return recovering, proof
}

func TestAdoptRecoveredClaimPreservesExpiredLease(t *testing.T) {
	claim, proof := recoveringClaimFixture(t)
	acceptance := id.New(coordination.PrefixTaskAcceptance)
	completed, err := coordination.AdoptRecoveredClaim(claim, claim.Token(), acceptance, proof, claim.At.Add(time.Second))
	require.NoError(t, err)
	require.NoError(t, completed.Validate())
	require.NoError(t, coordination.ValidateClaimSuccessor(claim, completed))
	require.Equal(t, claim.ExpiresAt, completed.ExpiresAt)
	require.Equal(t, claim.Fence, completed.Fence)
	require.Equal(t, claim.RunID, completed.RunID)
	require.Equal(t, claim.Digest, completed.PreviousDigest)
	_, digest, err := proof.Freeze()
	require.NoError(t, err)
	require.Equal(t, digest, completed.RecoveryDigest)
	_, err = coordination.CompleteClaim(claim, claim.Token(), acceptance, claim.At)
	require.Error(t, err)
	_, err = coordination.RenewClaim(completed, completed.Token(), time.Minute, completed.At)
	require.Error(t, err)
	changed := completed
	changed.RecoveryDigest = ""
	require.Error(t, changed.Validate())
}

func TestAdoptRecoveredClaimRejectsChangedOrExpiredProof(t *testing.T) {
	for _, mode := range []string{"claim", "run", "scope", "seal", "snapshot", "issuer", "proof", "future", "expired", "deadline", "token", "not recovering"} {
		t.Run(mode, func(t *testing.T) {
			claim, proof := recoveringClaimFixture(t)
			token := claim.Token()
			at := claim.At.Add(time.Second)
			switch mode {
			case "claim":
				proof.ClaimDigest = evidence.Digest([]byte("different"))
			case "run":
				proof.Seal.Request.RunID = id.NewAgentRunID()
			case "scope":
				proof.Seal.Request.Scope.Levels[0].Value = "different"
			case "seal":
				proof.Seal.RequestDigest = evidence.Digest(nil)
			case "snapshot":
				proof.SnapshotDigest = ""
			case "issuer":
				proof.Issuer = ""
			case "proof":
				proof.ProofDigest = ""
			case "future":
				at = proof.ObservedAt.Add(-time.Second)
			case "expired":
				at = proof.ExpiresAt
			case "deadline":
				at = claim.Deadline
			case "token":
				token.Revision--
			case "not recovering":
				claim.State = coordination.ClaimDispatching
			}
			got, err := coordination.AdoptRecoveredClaim(claim, token, id.New(coordination.PrefixTaskAcceptance), proof, at)
			require.Error(t, err)
			require.Empty(t, got)
		})
	}
}

func TestTaskRecoveryProofDetachesScope(t *testing.T) {
	_, proof := recoveringClaimFixture(t)
	frozen, digest, err := proof.Freeze()
	require.NoError(t, err)
	proof.Seal.Request.Scope.Levels[0].Value = "mutated"
	_, again, err := frozen.Freeze()
	require.NoError(t, err)
	require.Equal(t, digest, again)
}
