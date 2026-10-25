package coordination_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

func taskAcceptanceFixture(t *testing.T, recovery ...bool) coordination.TaskAcceptance {
	t.Helper()
	accepted, _ := taskAcceptanceClaimFixture(t, recovery...)
	return accepted
}

func taskAcceptanceClaimFixture(t *testing.T, recovery ...bool) (coordination.TaskAcceptance, coordination.Claim) {
	t.Helper()
	_, assignment, binding, p, now := reviewFixture(t)
	h := assignment.Handoff
	claim, err := coordination.AcquireClaim(nil, coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}, p, now)
	require.NoError(t, err)
	claim, err = coordination.BeginDispatch(claim, claim.Token(), binding.RunID, now)
	require.NoError(t, err)
	req := coordination.TaskAcceptanceRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixTaskAcceptance), Claim: claim.Token(), AssignmentID: h.AssignmentID, AssignmentRevision: assignment.Revision, AssignmentDigest: assignment.Digest, Result: verification.ResultReadRequest{SchemaVersion: 1, RunID: binding.RunID, ResultID: id.New(verification.PrefixResult), ReceiptDigest: evidence.Digest([]byte("receipt"))}, Current: binding, Actor: h.Sender.Principal, Reason: "verified"}
	if len(recovery) > 0 && recovery[0] {
		claim, err = coordination.ExpireClaim(claim, claim.Revision, claim.ExpiresAt)
		require.NoError(t, err)
		now = claim.At
		req.Claim = claim.Token()
		sealReq := execution.RunCallSealRequest{SchemaVersion: 1, Scope: claim.Scope, RunID: claim.RunID, Actor: "recovery", Reason: "closed"}
		_, sealDigest, sealErr := sealReq.Freeze()
		require.NoError(t, sealErr)
		req.Recovery = &coordination.TaskRecoveryProof{SchemaVersion: 1, ClaimDigest: claim.Digest, Seal: execution.RunCallSeal{Request: sealReq, RequestDigest: sealDigest, At: now}, SnapshotDigest: evidence.Digest([]byte("snapshot")), Issuer: "fixture", Revision: "1", ProofRef: "stopped-worker", ProofDigest: evidence.Digest([]byte("proof")), ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
		snapshot, snapshotErr := execution.SealRunRecoverySnapshot(execution.RunRecoverySnapshot{SchemaVersion: 1, Seal: req.Recovery.Seal})
		require.NoError(t, snapshotErr)
		req.Recovery.SnapshotDigest = snapshot.Digest
	}
	req, digest, err := req.Freeze()
	require.NoError(t, err)
	var completed coordination.Claim
	if req.Recovery != nil {
		completed, err = coordination.AdoptRecoveredClaim(claim, req.Claim, req.ID, *req.Recovery, now)
	} else {
		completed, err = coordination.CompleteClaim(claim, req.Claim, req.ID, now)
	}
	require.NoError(t, err)
	accepted, err := coordination.SealTaskAcceptance(coordination.TaskAcceptance{SchemaVersion: 1, Request: req, RequestDigest: digest, Claim: completed, ObservedAt: now, Outbox: coordination.TaskAcceptanceOutbox{EventID: id.New(coordination.PrefixTaskAcceptedEvent), Type: "task_result_accepted", AcceptanceID: req.ID, PlanID: p.ID, TaskID: h.TaskID, RunID: binding.RunID}})
	require.NoError(t, err)
	return accepted, claim
}

func TestTaskAcceptanceDetachesBindingsAndNeverPersistsFresh(t *testing.T) {
	original := taskAcceptanceFixture(t)
	original.Fresh = true
	frozen, err := coordination.SealTaskAcceptance(original)
	require.NoError(t, err)
	require.False(t, frozen.Fresh)
	original.Request.Current.Scope.Levels[0].Value = "other"
	*original.Claim.AcceptanceID = id.New(coordination.PrefixTaskAcceptance)
	require.NoError(t, frozen.Validate())
	frozen.Fresh = true
	data, err := json.Marshal(frozen)
	require.NoError(t, err)
	require.NotContains(t, string(data), "fresh")
	var roundtrip coordination.TaskAcceptance
	require.NoError(t, json.Unmarshal(data, &roundtrip))
	require.False(t, roundtrip.Fresh)
	require.NoError(t, roundtrip.Validate())
}

func TestTaskAcceptanceRejectsChangedReceiptAndCrossBindings(t *testing.T) {
	for _, mode := range []string{"request", "claim", "outbox", "time", "receipt", "result_run", "dependency_scope", "self_dependency"} {
		t.Run(mode, func(t *testing.T) {
			accepted := taskAcceptanceFixture(t)
			switch mode {
			case "request":
				accepted.Request.Reason = "changed"
			case "claim":
				accepted.Claim.Fence++
			case "outbox":
				accepted.Outbox.TaskID = id.New(accepted.Claim.TaskID.Prefix())
			case "time":
				accepted.ObservedAt = accepted.ObservedAt.Add(time.Second)
			case "receipt":
				accepted.Digest = evidence.Digest([]byte("tampered"))
			case "result_run":
				accepted.Request.Result.RunID = id.NewAgentRunID()
			case "dependency_scope":
				dep := coordination.AcceptedDependency{AcceptanceID: id.New(coordination.PrefixTaskAcceptance), Digest: accepted.Digest, Current: accepted.Request.Current}
				dep.Current.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}}
				accepted.Request.Dependencies = []coordination.AcceptedDependency{dep}
			case "self_dependency":
				accepted.Request.Dependencies = []coordination.AcceptedDependency{{AcceptanceID: accepted.Request.ID, Digest: accepted.Digest, Current: accepted.Request.Current}}
			}
			require.ErrorIs(t, accepted.Validate(), coordination.ErrInvalidTaskAcceptance)
		})
	}
}
