package execution

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

func TestContinuationRejectsUnprovedOrUnboundedRetry(t *testing.T) {
	r := continuationFixture()
	_, _, err := r.Freeze()
	require.NoError(t, err)
	for _, change := range []func(*EffectContinuationRequest){
		func(r *EffectContinuationRequest) { r.Proof.Mode = "unknown" },
		func(r *EffectContinuationRequest) { r.Operation.OperationKey = "new key" },
		func(r *EffectContinuationRequest) { r.Operation.ArgsDigest = evidence.Digest(nil) },
		func(r *EffectContinuationRequest) { r.Operation.RunCall = r.Original.RunCall },
		func(r *EffectContinuationRequest) { r.Reservation.Allocation.ToolCalls = 0 },
		func(r *EffectContinuationRequest) { r.Reservation.ParentID = id.New(PrefixReservation) },
		func(r *EffectContinuationRequest) { r.Sequence = 16 },
	} {
		candidate, _, _ := r.Freeze()
		change(&candidate)
		_, _, err = candidate.Freeze()
		require.Error(t, err)
	}
}
func continuationFixture() EffectContinuationRequest {
	original := effectRequest()
	original.Requirement.Retry = RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Second}
	call := RunCallRequest{SchemaVersion: 1, ID: id.New(PrefixRunCall), Scope: original.Scope, RunID: original.RunID, AgentID: id.NewAgentID(), ClaimID: id.New("tclaim"), ParentRunID: id.NewAgentRunID(), Kind: RunCallTool, OperationDigest: evidence.Digest([]byte("slot")), RequestDigest: evidence.Digest([]byte("request")), PreparedDigest: evidence.Digest([]byte("prepared")), AssignmentDigest: evidence.Digest([]byte("assignment")), Deadline: original.Deadline}
	original.RunCall = &call
	op := original
	op.RunCall = nil
	op.AttemptID = id.New(PrefixEffectAttempt)
	sealReq := RunCallSealRequest{SchemaVersion: 1, Scope: original.Scope, RunID: original.RunID, Actor: "host", Reason: "recovery"}
	_, sealDigest, _ := sealReq.Freeze()
	now := time.Now().UTC()
	return EffectContinuationRequest{SchemaVersion: 1, ResultID: id.New(evidence.PrefixArtifact), ResultRetention: 24 * time.Hour, Original: original, Operation: op, Sequence: 1, Action: EffectActionRetry, ClaimDigest: evidence.Digest([]byte("claim")), Seal: RunCallSeal{Request: sealReq, RequestDigest: sealDigest, At: now.Add(-time.Second)}, SnapshotDigest: evidence.Digest([]byte("snapshot")), PredecessorDigest: evidence.Digest([]byte("predecessor")), Proof: EffectContinuationProof{Mode: ContinuationAbsence, Issuer: "provider", Revision: "v1", Reference: "proof", Digest: evidence.Digest([]byte("proof")), ObservedAt: now, ExpiresAt: now.Add(time.Minute)}, Reservation: ReservationRequest{SchemaVersion: 1, ID: id.New(PrefixReservation), PlanID: id.New("plan"), TaskID: id.New("ptask"), PlanDigest: evidence.Digest([]byte("plan")), Scope: original.Scope, OwnerRef: op.AttemptID.String(), Kind: ReservationLeaf, Allocation: Resources{ToolCalls: 1, FanOut: 1}, Deadline: op.Deadline}}
}
