package storetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func testContinuationRetirement(t *testing.T, backend store.Store, driver assurance.Driver) {
	f, r := effectContinuationFixture(t, backend, driver, execution.EffectNonIdempotent, false)
	current, err := backend.GetRun(f.ctx, f.effect.RunID)
	require.NoError(t, err)
	current.State = run.StateFailed
	current.StopReason = run.StopExecutionError
	current.ResultRef = ""
	current.VerificationStatus = verification.StatusFailed
	ended := f.clock.Now()
	current.CompletedAt = &ended
	require.NoError(t, backend.UpdateRun(f.ctx, current))
	current, err = backend.GetRun(f.ctx, current.ID)
	require.NoError(t, err)
	usage := coordination.TaskUsageRequest{Claim: f.request.Claim, Seal: r.Seal, SnapshotDigest: r.SnapshotDigest}
	snapshot, err := f.repo.SnapshotTaskUsage(f.ctx, usage)
	require.NoError(t, err)
	closure := coordination.TaskUsageClosureRequest{SchemaVersion: 1, Usage: usage, UsageDigest: snapshot.Digest, Actor: "host", Reason: "failed task", Termination: &coordination.TaskUsageTermination{RunRevision: current.Revision, State: current.State, CompletedAt: *current.CompletedAt}}
	closed, err := f.repo.CloseTaskUsage(f.ctx, closure)
	require.NoError(t, err)
	settled, err := f.repo.SettleTaskUsage(f.ctx, terminalSettlementRequest(t, f, closure, closed))
	require.NoError(t, err)
	recovery := *f.request.Recovery
	recovery.ObservedAt = f.clock.Now()
	retirement := coordination.TaskRetirementRequest{SchemaVersion: 1, Claim: usage.Claim, Recovery: recovery, ClosureDigest: closed.Digest, SettlementDigest: settled.Digest, Actor: "host", Reason: "retire after continuation"}
	admission, err := f.repo.AdmitEffectContinuation(f.ctx, r)
	require.NoError(t, err)
	require.True(t, admission.Fresh)
	_, err = f.repo.RetireTaskClaim(f.ctx, retirement)
	require.ErrorIs(t, err, execution.ErrEffectUncertain, "original settlement does not release pending follow-up")
	f.clock.advance(time.Second)
	result, err := f.repo.CompleteEffectContinuation(f.ctx, r, continuationReceipt(r.Operation, false))
	require.NoError(t, err)
	_, err = f.repo.RetireTaskClaim(f.ctx, retirement)
	require.ErrorIs(t, err, execution.ErrEffectUncertain, "proof before follow-up completion is stale")
	retirement.Recovery.ObservedAt = result.At
	retired, err := f.repo.RetireTaskClaim(f.ctx, retirement)
	require.NoError(t, err)
	plan, err := f.repo.GetPlan(f.ctx, r.Reservation.PlanID)
	require.NoError(t, err)
	claim := retired.Claim
	next, err := f.repo.AcquireTaskClaim(f.ctx, coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: plan.ID, PlanRevision: plan.Revision, PlanDigest: plan.Digest, TaskID: claim.TaskID, ExpectedRevision: claim.Revision, Worker: claim.Worker, GrantRef: claim.GrantRef, Duration: time.Minute})
	require.NoError(t, err)
	require.NotEqual(t, claim.ID, next.ID)
	nextAttempt := nextContinuation(t, f, r, &result.Receipt)
	_, err = f.repo.AdmitEffectContinuation(f.ctx, nextAttempt)
	require.ErrorIs(t, err, coordination.ErrClaimConflict)
	replay, err := f.repo.AdmitEffectContinuation(f.ctx, r)
	require.NoError(t, err)
	require.False(t, replay.Fresh)
	retained, err := f.repo.GetEffectContinuationResult(f.ctx, r)
	require.NoError(t, err)
	require.Equal(t, &result, retained)
}
