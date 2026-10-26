package storetest

import (
	"context"
	"encoding/json"
	"sync"
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
)

func planReplanFixture(t *testing.T, backend store.Store) (context.Context, coordination.PlanReplanRequest) {
	t.Helper()
	ctx, p, h := assignmentFixture(t, backend)
	budgets := resultCapability[execution.BudgetStore](t, backend)
	policy := execution.ReservationPolicy{SchemaVersion: 1, PlanID: p.ID, Scope: p.Scope, Limits: execution.Resources{Steps: 10, Tokens: 1000, ToolCalls: 10, FanOut: 10, Replans: 2}, Deadline: p.Budget.Deadline, TokenOnly: true}
	require.NoError(t, budgets.CreateReservationPolicy(ctx, policy))
	account, err := budgets.GetBudgetSnapshot(ctx, p.ID)
	require.NoError(t, err)
	now := time.Now().UTC()
	attempt := id.New(coordination.PrefixPlanReplan)
	req := coordination.PlanReplanRequest{SchemaVersion: 1, ID: attempt, Progress: planning.ProgressSnapshot{SchemaVersion: 1, Plan: p, At: now, StartedAt: now.Add(-time.Hour), LastProgressAt: now.Add(-time.Hour), Account: account}, Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: h.TaskID, OwnerRef: attempt.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 1, Tokens: 10, FanOut: 1, Replans: 1}, Deadline: p.Budget.Deadline}, RunID: id.NewAgentRunID(), SessionID: id.NewSessionID(), AgentID: mustCreateAgent(t, backend, ctx, "replanner"), PreparedDigest: evidence.Digest([]byte("prepared")), InputDigest: evidence.Digest([]byte("input")), Actor: "host", Reason: "bounded replacement"}
	_, _, err = req.Freeze()
	require.NoError(t, err)
	return ctx, req
}
func createPlanReplanRun(ctx context.Context, t *testing.T, backend store.Store, a coordination.PlanReplanAdmission, output string) {
	t.Helper()
	now := time.Now().UTC()
	req := a.Request
	require.NoError(t, backend.CreateSession(ctx, &session.Session{ID: req.SessionID, AgentID: req.AgentID, Title: "Plan proposal"}))
	r := &run.Run{Entity: cortex.NewEntity(), ID: req.RunID, SessionID: req.SessionID, AgentID: req.AgentID, Scope: req.Progress.Plan.Scope, State: run.StateCompleted, Input: "input", Output: output, StartedAt: &now, CompletedAt: &now, Provenance: run.ExecutionProvenance{SchemaVersion: 1, Purpose: run.PurposeEvaluation, PreparedDigest: req.PreparedDigest}, Budget: execution.Budget{MaxSteps: 1, MaxTokens: 10, Deadline: req.Reservation.Deadline}, Usage: execution.Usage{Steps: 1, Tokens: 17}, StepCount: 1, TokensUsed: 17}
	require.NoError(t, backend.CreateRun(ctx, r))
}

// PlanReplanExecution exercises durable model admission, failure debt and guarded
// graph transitions in every backend, without claiming host proof qualification.
func PlanReplanExecution(t *testing.T, newStore func(*testing.T) store.Store) {
	t.Run("admission_race_and_unknown_ack", func(t *testing.T) {
		backend := newStore(t)
		ctx, req := planReplanFixture(t, backend)
		s := resultCapability[coordination.PlanReplanStore](t, backend)
		var wg sync.WaitGroup
		results := make(chan coordination.PlanReplanAdmission, 6)
		errs := make(chan error, 6)
		for range 6 {
			wg.Go(func() { a, err := s.AdmitPlanReplan(ctx, req); results <- a; errs <- err })
		}
		wg.Wait()
		close(results)
		close(errs)
		for err := range errs {
			require.NoError(t, err)
		}
		fresh := 0
		for a := range results {
			require.NoError(t, a.Validate())
			if a.Fresh {
				fresh++
			}
		}
		require.Equal(t, 1, fresh)
		retained, err := s.GetPlanReplan(ctx, req.ID)
		require.NoError(t, err)
		require.False(t, retained.Fresh)
		changed := req
		changed.ID = id.New(coordination.PrefixPlanReplan)
		changed.RunID = id.NewAgentRunID()
		changed.Reservation.ID = id.New(execution.PrefixReservation)
		changed.Reservation.OwnerRef = changed.ID.String()
		_, err = s.AdmitPlanReplan(ctx, changed)
		require.Error(t, err)
		out, err := s.ObservePlanReplan(ctx, coordination.PlanReplanControl{Request: req, Operation: "observe"})
		require.NoError(t, err)
		require.Nil(t, out)
		account, err := resultCapability[execution.BudgetStore](t, backend).GetBudgetSnapshot(ctx, req.Progress.Plan.ID)
		require.NoError(t, err)
		require.EqualValues(t, 1, account.Charged.Replans)
		require.EqualValues(t, 10, account.Charged.Tokens)
		require.Error(t, func() error { _, readErr := s.GetPlanReplan(context.Background(), req.ID); return readErr }())
		_, err = s.GetPlanReplan(cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}}), req.ID)
		require.NoError(t, err)
	})
	t.Run("malformed_output_retains_debt_and_overshoot", func(t *testing.T) {
		backend := newStore(t)
		ctx, req := planReplanFixture(t, backend)
		s := resultCapability[coordination.PlanReplanStore](t, backend)
		a, err := s.AdmitPlanReplan(ctx, req)
		require.NoError(t, err)
		createPlanReplanRun(ctx, t, backend, a, `{"unknown":"invalid"}`)
		out, err := s.ObservePlanReplan(ctx, coordination.PlanReplanControl{Request: req, Operation: "observe"})
		require.NoError(t, err)
		require.NotNil(t, out)
		require.NoError(t, out.Validate())
		require.Nil(t, out.Proposed)
		retry, err := s.ObservePlanReplan(ctx, coordination.PlanReplanControl{Request: req, Operation: "observe"})
		require.NoError(t, err)
		require.Equal(t, out.Digest, retry.Digest)
		account, err := resultCapability[execution.BudgetStore](t, backend).GetBudgetSnapshot(ctx, req.Progress.Plan.ID)
		require.NoError(t, err)
		require.EqualValues(t, 1, account.Charged.Replans)
		require.EqualValues(t, 17, account.Charged.Tokens)
		_, err = s.CommitPlanReplan(ctx, coordination.PlanReplanControl{Request: req, Operation: "commit"})
		require.Error(t, err)
		req.ID = id.New(coordination.PrefixPlanReplan)
		req.RunID = id.NewAgentRunID()
		req.Reservation.ID = id.New(execution.PrefixReservation)
		req.Reservation.OwnerRef = req.ID.String()
		req.Progress.Account = account
		req.Progress.At = time.Now().UTC()
		_, err = s.AdmitPlanReplan(ctx, req)
		require.NoError(t, err)
	})
	t.Run("proposal_commit_restart_and_historical_retry", func(t *testing.T) {
		backend := newStore(t)
		ctx, req := planReplanFixture(t, backend)
		s := resultCapability[coordination.PlanReplanStore](t, backend)
		a, err := s.AdmitPlanReplan(ctx, req)
		require.NoError(t, err)
		copyReq, _, err := req.Freeze()
		require.NoError(t, err)
		next := copyReq.Progress.Plan
		next.Tasks[0].Instruction = "another bounded approach"
		data, err := json.Marshal(next)
		require.NoError(t, err)
		createPlanReplanRun(ctx, t, backend, a, string(data))
		out, err := s.ObservePlanReplan(ctx, coordination.PlanReplanControl{Request: req, Operation: "observe"})
		require.NoError(t, err)
		require.NotNil(t, out.Proposed)
		control := coordination.PlanReplanControl{Request: req, Operation: "commit"}
		committed, err := s.CommitPlanReplan(ctx, control)
		require.NoError(t, err)
		require.EqualValues(t, 2, committed.Revision)
		retained, err := s.GetPlanReplanCommit(ctx, req.ID)
		require.NoError(t, err)
		require.Equal(t, committed.Digest, retained.Digest)
		committed.Tasks[0].Instruction = "third approach"
		plans := resultCapability[planning.Store](t, backend)
		_, err = plans.CommitRevision(ctx, planning.Revision{Plan: committed, Actor: "host", Reason: "new revision"}, committed.Revision)
		require.NoError(t, err)
		retried, err := s.CommitPlanReplan(ctx, control)
		require.NoError(t, err)
		require.EqualValues(t, 2, retried.Revision)
		replay, err := s.AdmitPlanReplan(ctx, req)
		require.NoError(t, err)
		require.False(t, replay.Fresh)
	})
	t.Run("active_ownership_blocks_commit", func(t *testing.T) {
		backend := newStore(t)
		ctx, req := planReplanFixture(t, backend)
		s := resultCapability[coordination.PlanReplanStore](t, backend)
		a, err := s.AdmitPlanReplan(ctx, req)
		require.NoError(t, err)
		data, err := json.Marshal(req.Progress.Plan)
		require.NoError(t, err)
		createPlanReplanRun(ctx, t, backend, a, string(data))
		_, err = s.ObservePlanReplan(ctx, coordination.PlanReplanControl{Request: req, Operation: "observe"})
		require.NoError(t, err)
		claims := resultCapability[coordination.ClaimStore](t, backend)
		_, err = claims.AcquireTaskClaim(ctx, coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: req.Progress.Plan.ID, PlanRevision: req.Progress.Plan.Revision, PlanDigest: req.Progress.Plan.Digest, TaskID: req.Progress.Plan.Tasks[0].ID, Worker: coordination.Identity{Agent: "worker", Principal: "worker"}, GrantRef: "grant", Duration: time.Minute})
		require.NoError(t, err)
		_, err = s.CommitPlanReplan(ctx, coordination.PlanReplanControl{Request: req, Operation: "commit"})
		require.Error(t, err)
		current, err := resultCapability[planning.Store](t, backend).GetPlan(ctx, req.Progress.Plan.ID)
		require.NoError(t, err)
		require.EqualValues(t, 1, current.Revision)
	})
	t.Run("permanent_abandonment_retains_unknown_charge", func(t *testing.T) {
		backend := newStore(t)
		ctx, req := planReplanFixture(t, backend)
		s := resultCapability[coordination.PlanReplanStore](t, backend)
		a, err := s.AdmitPlanReplan(ctx, req)
		require.NoError(t, err)
		now := time.Now().UTC()
		proof := &coordination.PlanReplanAbandonment{AdmissionDigest: a.RequestDigest, Issuer: "worker-registry", ProofRef: "permanent-fence", ProofDigest: evidence.Digest([]byte("proof")), ObservedAt: now, ExpiresAt: now.Add(time.Minute)}
		out, err := s.ObservePlanReplan(ctx, coordination.PlanReplanControl{Request: req, Operation: "observe", Abandonment: proof})
		require.NoError(t, err)
		require.NotNil(t, out.Abandonment)
		require.Nil(t, out.Proposed)
		account, err := resultCapability[execution.BudgetStore](t, backend).GetBudgetSnapshot(ctx, req.Progress.Plan.ID)
		require.NoError(t, err)
		require.EqualValues(t, 1, account.Charged.Replans)
		require.EqualValues(t, 10, account.Charged.Tokens)
	})
}
