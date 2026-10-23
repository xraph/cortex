package coordination_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

func TestTaskClaimLeaseAndFence(t *testing.T) {
	h, p, _, now := handoffFixture(t)
	request := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}
	claim, err := coordination.AcquireClaim(nil, request, p, now)
	require.NoError(t, err)
	require.Equal(t, uint64(1), claim.Fence)
	require.NoError(t, claim.Validate())
	require.Equal(t, coordination.ClaimHeld, claim.State)
	other := request
	other.ID = id.New(coordination.PrefixClaim)
	other.ExpectedRevision = claim.Revision
	_, err = coordination.AcquireClaim(&claim, other, p, now.Add(time.Second))
	require.ErrorIs(t, err, coordination.ErrClaimConflict)
	token := claim.Token()
	renewed, err := coordination.RenewClaim(claim, token, time.Minute, now.Add(30*time.Second))
	require.NoError(t, err)
	require.Equal(t, claim.Fence, renewed.Fence)
	require.Equal(t, claim.Revision+1, renewed.Revision)
	_, err = coordination.RenewClaim(renewed, token, time.Minute, now.Add(31*time.Second))
	require.ErrorIs(t, err, coordination.ErrClaimConflict)
	expired, err := coordination.ExpireClaim(renewed, renewed.Revision, renewed.ExpiresAt)
	require.NoError(t, err)
	require.Equal(t, coordination.ClaimReleased, expired.State)
	other.ExpectedRevision = expired.Revision
	successor, err := coordination.AcquireClaim(&expired, other, p, expired.At)
	require.NoError(t, err)
	require.Equal(t, expired.Fence+1, successor.Fence)
	_, err = coordination.BeginDispatch(successor, renewed.Token(), id.NewAgentRunID(), successor.At)
	require.ErrorIs(t, err, coordination.ErrClaimConflict)
}

func TestDispatchedClaimExpiryRequiresRecovery(t *testing.T) {
	h, p, _, now := handoffFixture(t)
	request := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}
	held, err := coordination.AcquireClaim(nil, request, p, now)
	require.NoError(t, err)
	child := id.NewAgentRunID()
	active, err := coordination.BeginDispatch(held, held.Token(), child, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, child, active.RunID)
	expired, err := coordination.ExpireClaim(active, active.Revision, active.ExpiresAt)
	require.NoError(t, err)
	require.Equal(t, coordination.ClaimRecovering, expired.State)
	require.Equal(t, child, expired.RunID)
	request.ID = id.New(coordination.PrefixClaim)
	request.ExpectedRevision = expired.Revision
	_, err = coordination.AcquireClaim(&expired, request, p, expired.At)
	require.ErrorIs(t, err, coordination.ErrClaimConflict)
	_, err = coordination.RenewClaim(expired, expired.Token(), time.Minute, expired.At)
	require.Error(t, err)
}

func TestClaimRejectsInvalidBindings(t *testing.T) {
	for _, mode := range []string{"plan", "revision", "digest", "task", "worker", "grant", "duration", "deadline", "schema"} {
		t.Run(mode, func(t *testing.T) {
			h, p, _, now := handoffFixture(t)
			request := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}
			switch mode {
			case "plan":
				request.PlanID = id.New(coordination.PrefixAssignment)
			case "revision":
				request.PlanRevision++
			case "digest":
				request.PlanDigest = "bad"
			case "task":
				request.TaskID = id.New(coordination.PrefixAssignment)
			case "worker":
				request.Worker.Principal = ""
			case "grant":
				request.GrantRef = ""
			case "duration":
				request.Duration = 31 * time.Minute
			case "deadline":
				now = p.Budget.Deadline
			case "schema":
				request.SchemaVersion = 2
			}
			_, err := coordination.AcquireClaim(nil, request, p, now)
			require.Error(t, err)
		})
	}
}

func TestClaimCannotMoveBackToOlderPlan(t *testing.T) {
	h, p, _, now := handoffFixture(t)
	newer, err := planning.ApplyRevision(p, planning.Revision{Plan: p, Actor: "host", Reason: "revised"}, p.Revision, planning.Limits{})
	require.NoError(t, err)
	request := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: newer.ID, PlanRevision: newer.Revision, PlanDigest: newer.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}
	claim, err := coordination.AcquireClaim(nil, request, newer, now)
	require.NoError(t, err)
	released, err := coordination.ExpireClaim(claim, claim.Revision, claim.ExpiresAt)
	require.NoError(t, err)
	request.ID, request.PlanRevision, request.PlanDigest, request.ExpectedRevision = id.New(coordination.PrefixClaim), p.Revision, p.Digest, released.Revision
	_, err = coordination.AcquireClaim(&released, request, p, released.At)
	require.ErrorIs(t, err, coordination.ErrClaimConflict)
}

func TestVerifiedClaimCompletionFencesFurtherWork(t *testing.T) {
	h, p, _, now := handoffFixture(t)
	request := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}
	held, err := coordination.AcquireClaim(nil, request, p, now)
	require.NoError(t, err)
	encoded, err := json.Marshal(held)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "acceptance_id", "new completion metadata must preserve existing claim digests")
	acceptanceID := id.New(coordination.PrefixTaskAcceptance)
	_, err = coordination.CompleteClaim(held, held.Token(), acceptanceID, now)
	require.Error(t, err)
	dispatched, err := coordination.BeginDispatch(held, held.Token(), id.NewAgentRunID(), now)
	require.NoError(t, err)
	completed, err := coordination.CompleteClaim(dispatched, dispatched.Token(), acceptanceID, now.Add(time.Second))
	require.NoError(t, err)
	require.Equal(t, coordination.ClaimCompleted, completed.State)
	require.NoError(t, coordination.ValidateClaimSuccessor(dispatched, completed))
	_, err = coordination.RenewClaim(completed, completed.Token(), time.Minute, now.Add(2*time.Second))
	require.Error(t, err)
	_, err = coordination.ExpireClaim(completed, completed.Revision, completed.ExpiresAt)
	require.Error(t, err)
	_, err = coordination.CompleteClaim(dispatched, held.Token(), acceptanceID, now.Add(time.Second))
	require.Error(t, err)
	_, err = coordination.CompleteClaim(dispatched, dispatched.Token(), acceptanceID, dispatched.ExpiresAt)
	require.Error(t, err)
}

func TestCompletedClaimNeedsNewPlanBeforeReassignment(t *testing.T) {
	h, p, _, now := handoffFixture(t)
	req := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: h.TaskID, Worker: h.Receiver, GrantRef: h.GrantRef, Duration: time.Minute}
	held, err := coordination.AcquireClaim(nil, req, p, now)
	require.NoError(t, err)
	dispatched, err := coordination.BeginDispatch(held, held.Token(), id.NewAgentRunID(), now)
	require.NoError(t, err)
	completed, err := coordination.CompleteClaim(dispatched, dispatched.Token(), id.New(coordination.PrefixTaskAcceptance), now)
	require.NoError(t, err)
	req.ID, req.ExpectedRevision = id.New(coordination.PrefixClaim), completed.Revision
	_, err = coordination.AcquireClaim(&completed, req, p, now)
	require.Error(t, err)
	next, err := planning.ApplyRevision(p, planning.Revision{Plan: p, Actor: "host", Reason: "new evidence required"}, p.Revision, planning.Limits{})
	require.NoError(t, err)
	req.PlanRevision, req.PlanDigest = next.Revision, next.Digest
	reassigned, err := coordination.AcquireClaim(&completed, req, next, now)
	require.NoError(t, err)
	require.Greater(t, reassigned.Fence, completed.Fence)
	require.NoError(t, coordination.ValidateClaimSuccessor(completed, reassigned))
}
