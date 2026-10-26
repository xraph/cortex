package storetest

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
)

func replanRecoveryFixture(t *testing.T, backend store.Store, driver assurance.Driver) (taskRecoveryFixture, coordination.PlanReplanRequest, coordination.TaskRecoveryProof) {
	t.Helper()
	option := taskRecoveryOptions{ProviderUsage: execution.ReservationUsage{Actual: execution.Resources{Tokens: 11}, Known: execution.KnownTokens}}
	option.Acquire = func(ctx context.Context, claim coordination.ClaimRequest, p planning.Plan) (coordination.Claim, error) {
		repo := assurance.New(driver)
		policy := execution.ReservationPolicy{SchemaVersion: 1, PlanID: p.ID, Scope: p.Scope, Limits: execution.Resources{Steps: 10, Tokens: 1000, ToolCalls: 10, FanOut: 10, Replans: 2, USDMicros: 10000}, Deadline: p.Budget.Deadline, PricingRevision: "prices-1"}
		require.NoError(t, repo.CreateReservationPolicy(ctx, policy))
		reservation := execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: claim.TaskID, OwnerRef: claim.ID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 1, Tokens: 100, ToolCalls: 1, FanOut: 1, USDMicros: 1000}, Deadline: policy.Deadline, MoneyBound: true, PricingRevision: policy.PricingRevision}
		admitted, err := repo.AcquireBudgetedTaskClaim(ctx, coordination.BudgetedClaimRequest{Claim: claim, Reservation: reservation})
		return admitted.Claim, err
	}
	f := newTaskRecoveryFixture(t, backend, driver, "", option)
	accepted, err := f.repo.AdoptTaskResult(f.ctx, f.request)
	require.NoError(t, err)
	usage := coordination.TaskUsageRequest{Claim: accepted.Claim.Token(), Seal: f.request.Recovery.Seal, SnapshotDigest: f.request.Recovery.SnapshotDigest}
	snapshot, err := f.repo.SnapshotTaskUsage(f.ctx, usage)
	require.NoError(t, err)
	closure := coordination.TaskUsageClosureRequest{SchemaVersion: 1, Usage: usage, UsageDigest: snapshot.Digest, Actor: "host", Reason: "closed acceptance"}
	closed, err := f.repo.CloseTaskUsage(f.ctx, closure)
	require.NoError(t, err)
	_, err = f.repo.SettleTaskUsage(f.ctx, terminalSettlementRequest(t, f, closure, closed))
	require.NoError(t, err)
	p, err := f.repo.GetPlan(f.ctx, accepted.Claim.PlanID)
	require.NoError(t, err)
	account, err := f.repo.GetBudgetSnapshot(f.ctx, p.ID)
	require.NoError(t, err)
	now := f.clock.Now()
	attempt := id.New(coordination.PrefixPlanReplan)
	req := coordination.PlanReplanRequest{SchemaVersion: 1, ID: attempt, Progress: planning.ProgressSnapshot{SchemaVersion: 1, Plan: p, At: now, StartedAt: now.Add(-time.Hour), LastProgressAt: now.Add(-time.Hour), Account: account}, Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: accepted.Claim.TaskID, OwnerRef: attempt.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 1, Tokens: 10, FanOut: 1, Replans: 1, USDMicros: 10}, Deadline: p.Budget.Deadline, MoneyBound: true, PricingRevision: "prices-1"}, RunID: id.NewAgentRunID(), SessionID: id.NewSessionID(), AgentID: mustCreateAgent(t, backend, f.ctx, "replanner"), PreparedDigest: evidence.Digest([]byte("prepared")), InputDigest: evidence.Digest([]byte("input")), Actor: "host", Reason: "bounded replacement"}
	proof := *f.request.Recovery
	proof.ClaimDigest = accepted.Claim.Digest
	proof.ObservedAt = now
	proof.ExpiresAt = now.Add(time.Minute)
	return f, req, proof
}

// PlanReplanRecovery verifies proof-bound commits against real retained claims,
// journals and accounting, including transaction rollback and uncertain admission.
func PlanReplanRecovery(t *testing.T, backend store.Store, driver assurance.Driver) {
	for _, mode := range []string{"resolved", "missing proof", "expired proof", "late return", "rollback", "lost acknowledgment"} {
		t.Run(mode, func(t *testing.T) {
			f, req, proof := replanRecoveryFixture(t, backend, driver)
			repo := f.repo
			if mode == "rollback" || mode == "lost acknowledgment" {
				plans, ok := driver.(assurance.PlanDriver)
				require.True(t, ok)
				fault := &submissionFenceDriver{Driver: driver, plans: plans}
				if mode == "rollback" {
					fault.afterInsert = func(_ context.Context, row assurance.Record) error {
						if row.Kind == "plan_replan" {
							return errAssignmentInjected
						}
						return nil
					}
				} else {
					fault.lostAck = true
				}
				repo = assurance.New(fault).WithClock(f.clock)
			}
			admission, err := repo.AdmitPlanReplan(f.ctx, req)
			if mode == "rollback" || mode == "lost acknowledgment" {
				require.Error(t, err)
				require.Empty(t, admission)
				retained, readErr := f.repo.GetPlanReplan(f.ctx, req.ID)
				require.NoError(t, readErr)
				if mode == "rollback" {
					require.Nil(t, retained)
					_, readErr = f.repo.GetReservation(f.ctx, req.Reservation.ID)
					require.ErrorIs(t, readErr, execution.ErrReservationNotFound)
				} else {
					require.NotNil(t, retained)
					retry, retryErr := f.repo.AdmitPlanReplan(f.ctx, req)
					require.NoError(t, retryErr)
					require.False(t, retry.Fresh)
				}
				return
			}
			require.NoError(t, err)
			// Persist a later instant at the coarsest supported run timestamp precision.
			// Admission JSON retains nanoseconds; Mongo run dates retain milliseconds.
			f.clock.advance(time.Second)
			now := f.clock.Now().Truncate(time.Millisecond)
			require.True(t, now.After(admission.At))
			data, err := json.Marshal(req.Progress.Plan)
			require.NoError(t, err)
			require.NoError(t, backend.CreateSession(f.ctx, &session.Session{ID: req.SessionID, AgentID: req.AgentID, Title: "Proposal"}))
			proposal := &run.Run{Entity: cortex.NewEntity(), ID: req.RunID, AgentID: req.AgentID, SessionID: req.SessionID, Scope: req.Progress.Plan.Scope, State: run.StateCompleted, Input: "input", Output: string(data), StartedAt: &now, CompletedAt: &now, Provenance: run.ExecutionProvenance{SchemaVersion: 1, Purpose: run.PurposeEvaluation, PreparedDigest: req.PreparedDigest}, Budget: execution.Budget{MaxSteps: 1, MaxTokens: 10, Deadline: req.Reservation.Deadline}}
			require.NoError(t, backend.CreateRun(f.ctx, proposal))
			out, err := f.repo.ObservePlanReplan(f.ctx, coordination.PlanReplanControl{Request: req, Operation: "observe"})
			require.NoError(t, err)
			require.NotNil(t, out.Proposed)
			control := coordination.PlanReplanControl{Request: req, Operation: "commit", Claims: []coordination.TaskRecoveryProof{proof}}
			switch mode {
			case "missing proof":
				control.Claims = nil
			case "expired proof":
				f.clock.advance(2 * time.Minute)
			case "late return":
				_, digest, freezeErr := f.model.Freeze()
				require.NoError(t, freezeErr)
				require.NoError(t, f.repo.CompleteRunCall(f.ctx, f.model, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("contradiction"))}))
			}
			committed, err := f.repo.CommitPlanReplan(f.ctx, control)
			if mode != "resolved" {
				require.Error(t, err)
				current, readErr := f.repo.GetPlan(f.ctx, req.Progress.Plan.ID)
				require.NoError(t, readErr)
				require.EqualValues(t, 1, current.Revision)
				return
			}
			require.NoError(t, err)
			require.EqualValues(t, 2, committed.Revision)
			retained, err := f.repo.GetTaskClaim(f.ctx, req.Progress.Plan.ID, req.Progress.Plan.Tasks[0].ID)
			require.NoError(t, err)
			require.Equal(t, coordination.ClaimCompleted, retained.State)
			require.Equal(t, req.Progress.Plan.Digest, retained.PlanDigest, "acceptance is not silently transferred across plans")
		})
	}
}
