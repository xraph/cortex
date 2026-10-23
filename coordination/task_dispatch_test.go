package coordination_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

func taskDispatchFixture(t *testing.T) (coordination.TaskDispatchRequest, coordination.TaskDispatch) {
	t.Helper()
	h, p, _, at := handoffFixture(t)
	claim, err := coordination.AcquireClaim(nil, coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}, p, at)
	require.NoError(t, err)
	h.Fence = claim.Fence
	h, err = coordination.SealHandoff(h)
	require.NoError(t, err)
	a, err := coordination.Propose(h, at)
	require.NoError(t, err)
	change := coordination.AssignmentChange{AssignmentID: h.AssignmentID, ExpectedRevision: a.Revision, HandoffDigest: h.Digest, Fence: h.Fence, State: coordination.AssignmentAccepted, Actor: h.Receiver.Principal, Reason: "accepted"}
	a, err = coordination.Advance(a, change, at)
	require.NoError(t, err)
	req := coordination.TaskDispatchRequest{SchemaVersion: 1, Scope: p.Scope, Claim: claim.Token(), ClaimDigest: claim.Digest, AdmissionDigest: evidence.Digest([]byte("admission")), AssignmentID: h.AssignmentID, AssignmentRevision: a.Revision, AssignmentDigest: a.Digest, ParentRunRevision: 1, RunID: id.NewAgentRunID(), Actor: h.Receiver.Principal, Reason: "dispatch"}
	_, digest, err := req.Freeze()
	require.NoError(t, err)
	next, err := coordination.BeginDispatch(claim, claim.Token(), req.RunID, at)
	require.NoError(t, err)
	change.ExpectedRevision, change.State, change.Reason = a.Revision, coordination.AssignmentWorking, req.Reason
	a, err = coordination.Advance(a, change, at)
	require.NoError(t, err)
	receipt := coordination.TaskDispatch{RequestDigest: digest, Claim: next, Assignment: a, Outbox: coordination.TaskDispatchOutbox{EventID: id.New(coordination.PrefixTaskDispatchEvent), Type: "task_dispatched", ClaimID: claim.ID, PlanID: p.ID, TaskID: h.TaskID, AssignmentID: h.AssignmentID, RunID: req.RunID}}
	require.NoError(t, receipt.Validate(req))
	return req, receipt
}

func TestTaskDispatchRequestValidation(t *testing.T) {
	for _, mode := range []string{"version", "scope", "claim", "claim_digest", "admission_digest", "assignment", "assignment_revision", "assignment_digest", "parent_revision", "run", "actor", "reason", "dependency"} {
		t.Run(mode, func(t *testing.T) {
			req, _ := taskDispatchFixture(t)
			switch mode {
			case "version":
				req.SchemaVersion++
			case "scope":
				req.Scope.Levels = nil
			case "claim":
				req.Claim.Fence = 0
			case "claim_digest":
				req.ClaimDigest = ""
			case "admission_digest":
				req.AdmissionDigest = ""
			case "assignment":
				req.AssignmentID = id.NewAgentID()
			case "assignment_revision":
				req.AssignmentRevision = 0
			case "assignment_digest":
				req.AssignmentDigest = ""
			case "parent_revision":
				req.ParentRunRevision = 0
			case "run":
				req.RunID = id.NewAgentID()
			case "actor":
				req.Actor = "other"
			case "reason":
				req.Reason = ""
			case "dependency":
				req.Dependencies = []coordination.AcceptedDependency{{}}
			}
			frozen, digest, err := req.Freeze()
			require.ErrorIs(t, err, coordination.ErrInvalidClaim)
			require.Zero(t, frozen)
			require.Empty(t, digest)
		})
	}
}

func TestTaskDispatchReceiptAssociation(t *testing.T) {
	for _, mode := range []string{"request", "run", "claim", "assignment", "event", "event_type", "event_run", "event_assignment", "event_claim", "event_plan", "event_task"} {
		t.Run(mode, func(t *testing.T) {
			req, receipt := taskDispatchFixture(t)
			_, unrelated := taskDispatchFixture(t)
			switch mode {
			case "request":
				receipt.RequestDigest = evidence.Digest([]byte("other"))
			case "run":
				req.RunID = id.NewAgentRunID()
				_, receipt.RequestDigest, _ = req.Freeze()
			case "claim":
				receipt.Claim = unrelated.Claim
			case "assignment":
				receipt.Assignment = unrelated.Assignment
			case "event":
				receipt.Outbox.EventID = id.NewAgentRunID()
			case "event_type":
				receipt.Outbox.Type = "completed"
			case "event_run":
				receipt.Outbox.RunID = id.NewAgentRunID()
			case "event_assignment":
				receipt.Outbox.AssignmentID = unrelated.Outbox.AssignmentID
			case "event_claim":
				receipt.Outbox.ClaimID = unrelated.Outbox.ClaimID
			case "event_plan":
				receipt.Outbox.PlanID = unrelated.Outbox.PlanID
			case "event_task":
				receipt.Outbox.TaskID = unrelated.Outbox.TaskID
			}
			require.ErrorIs(t, receipt.Validate(req), coordination.ErrInvalidClaim)
		})
	}
}

func TestTaskDispatchFreezeDetachesScopeAndDependencies(t *testing.T) {
	req, _ := taskDispatchFixture(t)
	acceptance := taskAcceptanceFixture(t)
	acceptance.Request.Current.Scope = req.Scope
	req.Dependencies = []coordination.AcceptedDependency{{AcceptanceID: acceptance.Request.ID, Digest: acceptance.Digest, Current: acceptance.Request.Current}}
	frozen, digest, err := req.Freeze()
	require.NoError(t, err)
	req.Scope.Levels[0].Value = "changed"
	req.Dependencies[0].Current.Context.AuthorityRevision = "changed"
	require.NotEqual(t, req.Scope, frozen.Scope)
	require.NotEqual(t, req.Dependencies, frozen.Dependencies)
	_, stable, err := frozen.Freeze()
	require.NoError(t, err)
	require.Equal(t, digest, stable)
}
