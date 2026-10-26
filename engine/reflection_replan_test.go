package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
)

type reflectionReplanSource func(context.Context, cognitive.ReflectionReceipt) (*coordination.PlanReplanRequest, error)

func (f reflectionReplanSource) ResolveReflectionReplan(ctx context.Context, r cognitive.ReflectionReceipt) (*coordination.PlanReplanRequest, error) {
	return f(ctx, r)
}
func TestReflectionReplanCannotInventSharedAllowance(t *testing.T) {
	f, e, client, prepared, task, admitter := preparedTaskRuntimeFixture(t)
	require.NoError(t, engine.WithTaskRunAdmitter(admitter)(e))
	require.NoError(t, engine.WithTaskCallGuard(runtimeTaskGuard(allowRuntimeTaskCall))(e))
	now := time.Now().UTC()
	proof, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: task.Run.Scope, MediaType: "application/json", Version: "v1", Producer: "host-checker", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte(`{"conflict":true}`)})
	require.NoError(t, err)
	require.NoError(t, f.storage.SaveEvidence(f.ctx, proof))
	host := &reflectionHost{observation: func(in engine.ReflectionObservationInput) cognitive.ReflectionSnapshot {
		s := observation(in)
		s.Assumptions = []cognitive.Assumption{{Key: "resource", Summary: "Resource assumption contradicted", Origin: cognitive.AssumptionHost, State: cognitive.AssumptionDisputed, Evidence: []evidence.Ref{proof.Ref}}}
		return s
	}}
	preparedCalls := 0
	runtime, err := e.NewPlanReplanRuntime(replanInput(func(context.Context, coordination.PlanReplanRequest) (*engine.PreparedAgent, string, error) {
		preparedCalls++
		return prepared, "proposal", nil
	}))
	require.NoError(t, err)
	auth := &replanAuthority{}
	replanner, err := coordination.NewPlanReplanner(f.storage, auth, auth, runtime)
	require.NoError(t, err)
	resolved := 0
	source := reflectionReplanSource(func(ctx context.Context, receipt cognitive.ReflectionReceipt) (*coordination.PlanReplanRequest, error) {
		resolved++
		require.Equal(t, task.Claim.PlanID, receipt.Request.Snapshot.Binding.SharedPlanID)
		plan, readErr := f.storage.GetPlan(ctx, task.Claim.PlanID)
		if readErr != nil {
			return nil, readErr
		}
		account, readErr := f.storage.GetBudgetSnapshot(ctx, plan.ID)
		if readErr != nil {
			return nil, readErr
		}
		require.Zero(t, account.Policy.Limits.Replans)
		// A dishonest resolver reports capacity that was never allocated. The
		// durable replanner must compare the real account before provider work.
		account.Policy.Limits.Replans = 2
		account.Policy.Limits.FanOut = 3
		account.Charged = execution.Resources{}
		at := time.Now().UTC()
		attempt := id.New(coordination.PrefixPlanReplan)
		req := coordination.PlanReplanRequest{SchemaVersion: 1, ID: attempt, Progress: planning.ProgressSnapshot{SchemaVersion: 1, Plan: plan, At: at, StartedAt: at.Add(-time.Hour), LastProgressAt: at.Add(-time.Hour), Account: account}, Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: plan.ID, PlanDigest: plan.Digest, Scope: plan.Scope, TaskID: task.Claim.TaskID, OwnerRef: attempt.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 1, Tokens: 10, FanOut: 1, Replans: 1}, Deadline: plan.Budget.Deadline}, RunID: id.NewAgentRunID(), SessionID: id.NewSessionID(), AgentID: task.Run.AgentID, PreparedDigest: prepared.Digest(), InputDigest: evidence.Digest([]byte("proposal")), Actor: "host", Reason: "bounded reflection"}
		_, _, freezeErr := req.Freeze()
		require.NoError(t, freezeErr)
		return &req, nil
	})
	require.NoError(t, engine.WithReflection(engine.ReflectionConfig{Policy: cognitive.ReflectionPolicy{Revision: "v1", OnStuck: cognitive.ActionReplan}, Observer: host, Fence: host, Authorizer: host, Replanner: replanner, ReplanSource: source})(e))
	result, err := e.RunPreparedTask(f.ctx, prepared, task)
	require.NoError(t, err)
	require.Equal(t, run.StateIncomplete, result.State)
	require.Equal(t, 1, resolved)
	require.Zero(t, preparedCalls)
	require.Zero(t, client.calls)
	receipt, err := f.storage.GetReflection(f.ctx, task.Run.ID)
	require.NoError(t, err)
	require.Equal(t, 1, receipt.State.Reflections)
	require.True(t, receipt.State.Pending)
	attempts, err := f.storage.ListPlanReplans(f.ctx, task.Claim.PlanID)
	require.NoError(t, err)
	require.Empty(t, attempts)
}
