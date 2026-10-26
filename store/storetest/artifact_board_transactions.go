package storetest

import (
	"context"
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

// ArtifactBoardTransactions covers atomic intent rollback and lost acknowledgments.
func ArtifactBoardTransactions(t *testing.T, s store.Store, driver assurance.Driver) {
	for _, mode := range []string{"rollback", "lost acknowledgment"} {
		t.Run(mode, func(t *testing.T) {
			ctx, requests := artifactBoardFixture(t, s)
			req := requests[0]
			plans, ok := driver.(assurance.PlanDriver)
			require.True(t, ok)
			fault := &submissionFenceDriver{Driver: driver, plans: plans, lostAck: mode == "lost acknowledgment"}
			if mode == "rollback" {
				fault.afterInsert = func(_ context.Context, row assurance.Record) error {
					if row.Kind == "coordination_event" {
						return errAssignmentInjected
					}
					return nil
				}
			}
			receipt, err := assurance.New(fault).WriteArtifactBoard(ctx, req)
			require.Error(t, err)
			require.Empty(t, receipt)
			repo := assurance.New(driver)
			history, err := repo.ReadArtifactBoard(ctx, coordination.ArtifactBoardRead{Scope: req.Scope, PlanID: req.PlanID, Resource: req.Resource, Limit: 128})
			require.NoError(t, err)
			events, err := repo.ListCoordinationEvents(ctx, coordination.EventPage{Scope: req.Scope, PlanID: req.PlanID, Limit: 256})
			require.NoError(t, err)
			if mode == "rollback" {
				require.Empty(t, history)
				require.Empty(t, events)
			} else {
				require.Len(t, history, 1)
				require.Len(t, events, 1)
			}
			replay, err := repo.WriteArtifactBoard(ctx, req)
			require.NoError(t, err)
			if mode != "rollback" {
				require.Equal(t, history[0], replay)
			}
			events, err = repo.ListCoordinationEvents(ctx, coordination.EventPage{Scope: req.Scope, PlanID: req.PlanID, Limit: 256})
			require.NoError(t, err)
			require.Len(t, events, 1)
		})
	}
	t.Run("effect history after successor", func(t *testing.T) { testCoordinationEffectHistory(t, s, driver) })
}
func testCoordinationEffectHistory(t *testing.T, s store.Store, driver assurance.Driver) {
	f, r := effectContinuationFixture(t, s, driver, execution.EffectNonIdempotent, false)
	current, err := s.GetRun(f.ctx, f.effect.RunID)
	require.NoError(t, err)
	current.State = run.StateFailed
	current.StopReason = run.StopExecutionError
	current.ResultRef = ""
	current.VerificationStatus = verification.StatusFailed
	ended := f.clock.Now()
	current.CompletedAt = &ended
	require.NoError(t, s.UpdateRun(f.ctx, current))
	current, err = s.GetRun(f.ctx, current.ID)
	require.NoError(t, err)
	usage := coordination.TaskUsageRequest{Claim: f.request.Claim, Seal: r.Seal, SnapshotDigest: r.SnapshotDigest}
	snapshot, err := f.repo.SnapshotTaskUsage(f.ctx, usage)
	require.NoError(t, err)
	closure := coordination.TaskUsageClosureRequest{SchemaVersion: 1, Usage: usage, UsageDigest: snapshot.Digest, Actor: "host", Reason: "failed task", Termination: &coordination.TaskUsageTermination{RunRevision: current.Revision, State: current.State, CompletedAt: *current.CompletedAt}}
	closed, err := f.repo.CloseTaskUsage(f.ctx, closure)
	require.NoError(t, err)
	settled, err := f.repo.SettleTaskUsage(f.ctx, terminalSettlementRequest(t, f, closure, closed))
	require.NoError(t, err)
	_, err = f.repo.AdmitEffectContinuation(f.ctx, r)
	require.NoError(t, err)
	result, err := f.repo.CompleteEffectContinuation(f.ctx, r, continuationReceipt(r.Operation, false))
	require.NoError(t, err)
	recovery := *f.request.Recovery
	recovery.ObservedAt = result.At
	retired, err := f.repo.RetireTaskClaim(f.ctx, coordination.TaskRetirementRequest{SchemaVersion: 1, Claim: usage.Claim, Recovery: recovery, ClosureDigest: closed.Digest, SettlementDigest: settled.Digest, Actor: "host", Reason: "retire resolved work"})
	require.NoError(t, err)
	plan, err := f.repo.GetPlan(f.ctx, r.Reservation.PlanID)
	require.NoError(t, err)
	claim := retired.Claim
	successor, err := f.repo.AcquireTaskClaim(f.ctx, coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: plan.ID, PlanRevision: plan.Revision, PlanDigest: plan.Digest, TaskID: claim.TaskID, ExpectedRevision: claim.Revision, Worker: claim.Worker, GrantRef: claim.GrantRef, Duration: time.Minute})
	require.NoError(t, err)
	require.NotEqual(t, claim.ID, successor.ID)
	for _, source := range []string{"task_claim_history", "effect_continuation_history"} {
		after := ""
		for pages := 0; ; pages++ {
			require.Less(t, pages, 32)
			page, importErr := f.repo.ImportCoordinationEvents(f.ctx, coordination.EventImport{Scope: plan.Scope, PlanID: plan.ID, Source: source, After: after, Limit: 1})
			require.NoError(t, importErr)
			if page.Next == "" {
				break
			}
			after = page.Next
		}
	}
	events, err := f.repo.ListCoordinationEvents(f.ctx, coordination.EventPage{Scope: plan.Scope, PlanID: plan.ID, Limit: 256})
	require.NoError(t, err)
	found := map[string]bool{}
	for _, event := range events {
		found[event.Type] = true
		if event.Type == "effect_retry_resolved" {
			require.Equal(t, claim.RunID, event.RunID)
			require.NotNil(t, event.Artifact)
			require.Equal(t, result.Evidence.ID, event.Artifact.ID)
		}
	}
	require.True(t, found["task_claim_retired"])
	require.True(t, found["effect_retry_admitted"])
	require.True(t, found["effect_retry_resolved"])
}
