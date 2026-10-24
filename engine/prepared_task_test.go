package engine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/verification"
)

func preparedTaskOptions(t *testing.T, f workerFixture, ag *agent.Config, budget execution.Budget) engine.PreparedTaskRunOptions {
	t.Helper()
	at := time.Now().UTC()
	worker := coordination.Identity{Agent: ag.ID.String(), Principal: f.request.Job.Binding.Context.PrincipalRef}
	h, err := coordination.SealHandoff(coordination.Handoff{SchemaVersion: 1, ID: id.New(coordination.PrefixHandoff), AssignmentID: id.New(coordination.PrefixAssignment), Fence: 1, Scope: cortex.ScopeFromContext(f.ctx), PlanID: id.New(planning.PrefixPlan), PlanRevision: 1, PlanDigest: evidence.Digest([]byte("plan")), TaskID: id.New(planning.PrefixTask), ParentRunID: id.NewAgentRunID(), Sender: coordination.Identity{Agent: "parent", Principal: "host"}, Receiver: worker, Purpose: coordination.HandoffWork, GrantRef: "grant", Authority: planning.Authority{Budget: budget}, ContractDigest: f.request.Job.Binding.ContractDigest, Criteria: []string{"correct"}, Deadline: budget.Deadline})
	require.NoError(t, err)
	a, err := coordination.Propose(h, at)
	require.NoError(t, err)
	for _, state := range []coordination.AssignmentState{coordination.AssignmentAccepted, coordination.AssignmentWorking} {
		a, err = coordination.Advance(a, coordination.AssignmentChange{AssignmentID: h.AssignmentID, ExpectedRevision: a.Revision, HandoffDigest: h.Digest, Fence: h.Fence, State: state, Actor: worker.Principal, Reason: "dispatch"}, at)
		require.NoError(t, err)
	}
	return engine.PreparedTaskRunOptions{Claim: coordination.ClaimToken{ClaimID: id.New(coordination.PrefixClaim), PlanID: h.PlanID, TaskID: h.TaskID, Worker: worker, Fence: h.Fence, Revision: 2}, DispatchDigest: evidence.Digest([]byte("dispatch")), Assignment: a, ParentRunRevision: 1, RunID: id.NewAgentRunID(), Reason: "prepare child"}
}

func TestPrepareTaskRunBindsReservedIdentityWithoutExecution(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredClient{}
	e, ag, overrides := requiredSetup(t, f, client)
	overrides.Budget.MaxToolCalls = 1
	p, err := e.PrepareAgent(f.ctx, ag.ID, overrides)
	require.NoError(t, err)
	opts := preparedTaskOptions(t, f, ag, *overrides.Budget)
	req, err := e.PrepareTaskRun(f.ctx, p, "private task", opts)
	require.NoError(t, err)
	_, _, err = req.Freeze()
	require.NoError(t, err)
	require.Equal(t, opts.RunID, req.Run.ID)
	require.Equal(t, opts.Assignment.Handoff.Digest, req.Run.Provenance.AssignmentDigest)
	require.Equal(t, opts.Assignment.Handoff.ParentRunID.String(), req.Run.Provenance.ParentRunID)
	require.Equal(t, p.Digest(), req.Run.Provenance.PreparedDigest)
	require.Zero(t, req.Run.Revision)
	require.True(t, req.Run.CreatedAt.IsZero())
	require.Nil(t, req.Run.StartedAt)
	require.Equal(t, 970, req.Run.Budget.MaxTokens)
	require.Zero(t, client.calls)
	require.Zero(t, f.adapter.evaluations)
	_, err = f.storage.GetRun(f.ctx, opts.RunID)
	require.ErrorIs(t, err, cortex.ErrRunNotFound)
	_, err = f.storage.GetRunCaptureIntent(f.ctx, opts.RunID)
	require.ErrorIs(t, err, verification.ErrNotFound)
	artifact, err := f.storage.LoadEvidence(f.ctx, *req.Intent.Template.Binding.Context.ExecutionInputs)
	require.NoError(t, err)
	inputs, err := verification.DecodeExecutionInputs(artifact.Data)
	require.NoError(t, err)
	require.Equal(t, opts.RunID, inputs.RunID)
	require.Equal(t, "private task", inputs.Input)
	require.Equal(t, req.Run.SessionID, inputs.SessionID)
	opts.Assignment.Handoff.Scope.Levels[0].Value = "mutated"
	require.NotEqual(t, opts.Assignment.Handoff.Scope, req.Run.Scope)
}

func TestPrepareTaskRunRejectsUnboundPreparation(t *testing.T) {
	for _, mode := range []string{"scope", "cancelled", "claim", "worker", "assignment", "parent", "run", "dispatch", "budget", "contract", "verification"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			client := &requiredClient{}
			e, ag, overrides := requiredSetup(t, f, client)
			overrides.Budget.MaxToolCalls = 1
			opts := preparedTaskOptions(t, f, ag, *overrides.Budget)
			switch mode {
			case "claim":
				opts.Claim.Fence++
			case "worker":
				opts.Claim.Worker.Agent = id.NewAgentID().String()
			case "assignment":
				opts.Assignment.State = coordination.AssignmentAccepted
			case "parent":
				opts.ParentRunRevision = 0
			case "run":
				opts.RunID = opts.Assignment.Handoff.ParentRunID
			case "dispatch":
				opts.DispatchDigest = ""
			case "budget":
				overrides.Budget.MaxSteps++
			case "contract":
				opts.Assignment.Handoff.ContractDigest = evidence.Digest([]byte("other contract"))
			case "verification":
				var err error
				e, err = engine.New(engine.WithStore(f.storage), engine.WithLLM(client))
				require.NoError(t, err)
			}
			p, err := e.PrepareAgent(f.ctx, ag.ID, overrides)
			require.NoError(t, err)
			ctx := f.ctx
			if mode == "scope" {
				ctx = context.Background()
			}
			if mode == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			req, err := e.PrepareTaskRun(ctx, p, "task", opts)
			require.Error(t, err)
			require.Zero(t, req)
			require.Zero(t, client.calls)
			rows, err := f.storage.ListRuns(f.ctx, &run.ListFilter{AgentID: ag.ID.String()})
			require.NoError(t, err)
			require.Empty(t, rows)
		})
	}
}

func TestPrepareTaskRunUsesScannedPrivateHistory(t *testing.T) {
	f := newWorkerFixture(t)
	client := &requiredToolClient{final: true}
	e := requiredToolEngine(t, f, client, &requiredToolAuthority{}, engine.WithExternalTool(llm.Tool{Name: "probe"}), engine.WithSafety(requiredInputScanner{}))
	ag, overrides := requiredToolAgent(t, f)
	opts := preparedTaskOptions(t, f, ag, *overrides.Budget)
	opts.SessionID = id.NewSessionID()
	require.NoError(t, f.storage.CreateSession(f.ctx, &session.Session{ID: opts.SessionID, AgentID: ag.ID}))
	require.NoError(t, f.storage.SaveConversation(f.ctx, ag.ID, opts.SessionID, []memory.Message{{Role: "user", Content: "secret history"}}))
	p, err := e.PrepareAgent(f.ctx, ag.ID, overrides)
	require.NoError(t, err)
	req, err := e.PrepareTaskRun(f.ctx, p, "secret task", opts)
	require.NoError(t, err)
	artifact, err := f.storage.LoadEvidence(f.ctx, *req.Intent.Template.Binding.Context.ExecutionInputs)
	require.NoError(t, err)
	inputs, err := verification.DecodeExecutionInputs(artifact.Data)
	require.NoError(t, err)
	require.Equal(t, "safe task", req.Run.Input)
	require.Equal(t, "safe task", inputs.Input)
	require.Equal(t, "safe history", inputs.History[0].Content)
	require.Equal(t, opts.SessionID, inputs.SessionID)
	require.NotContains(t, string(artifact.Data), "secret")
	require.Zero(t, client.calls)
}

func TestPrepareTaskRunRejectsChangedCaptureInputs(t *testing.T) {
	for _, mode := range []string{"input", "denied", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			f := newWorkerFixture(t)
			ctx, cancel := context.WithCancel(f.ctx)
			defer cancel()
			client := &requiredClient{}
			e, ag, overrides := requiredSetup(t, f, client, func(c *engine.RequiredVerificationConfig) {
				builder := c.Builder
				c.Builder = requiredBuilder(func(ctx context.Context, in engine.VerificationIntentInput) (verification.CaptureIntent, error) {
					if mode == "input" {
						in.Inputs.Input = "changed"
					}
					intent, err := builder.BuildVerificationIntent(ctx, in)
					if mode == "cancelled" {
						cancel()
					}
					return intent, err
				})
				if mode == "denied" {
					c.CaptureAuthorizer = captureAuthorizer(func(context.Context, evidence.Action, evidence.Ref) error { return cortex.ErrEvidenceAccessDenied })
				}
			})
			overrides.Budget.MaxToolCalls = 1
			opts := preparedTaskOptions(t, f, ag, *overrides.Budget)
			p, err := e.PrepareAgent(ctx, ag.ID, overrides)
			require.NoError(t, err)
			req, err := e.PrepareTaskRun(ctx, p, "task", opts)
			require.Error(t, err)
			require.Zero(t, req)
			require.Zero(t, client.calls)
			_, err = f.storage.GetRun(f.ctx, opts.RunID)
			require.ErrorIs(t, err, cortex.ErrRunNotFound)
		})
	}
}

type preparedTaskFence func(context.Context, coordination.TaskRunRequest, func(coordination.TaskRunRequest) error) error

func (f preparedTaskFence) WithTaskRunFence(ctx context.Context, r coordination.TaskRunRequest, next func(coordination.TaskRunRequest) error) error {
	return f(ctx, r, next)
}

type preparedTaskAuthorizer func(context.Context, coordination.TaskRunRequest) error

func (a preparedTaskAuthorizer) AuthorizeTaskRun(ctx context.Context, r coordination.TaskRunRequest) error {
	return a(ctx, r)
}

func preparedTaskRuntimeFixture(t *testing.T, configure ...func(*engine.Engine, *engine.RunOverrides)) (workerFixture, *engine.Engine, *requiredClient, *engine.PreparedAgent, coordination.TaskRunRequest, coordination.TaskRunAdmitter) {
	t.Helper()
	f := newWorkerFixture(t)
	client := &requiredClient{}
	var p planning.Plan
	var taskID id.ID
	e, ag, overrides := requiredSetup(t, f, client, func(c *engine.RequiredVerificationConfig) {
		c.HandoffAuthorizer = &requiredToolAuthority{}
		base := c.Builder
		c.Builder = requiredBuilder(func(ctx context.Context, in engine.VerificationIntentInput) (verification.CaptureIntent, error) {
			intent, err := base.BuildVerificationIntent(ctx, in)
			if err != nil {
				return intent, err
			}
			intent.Template.Binding.Candidate.Producer = planning.ArtifactProducer(p, taskID)
			return verification.SealCaptureIntent(intent)
		})
	})
	overrides.Budget.MaxToolCalls = 1
	for _, change := range configure {
		change(e, overrides)
	}
	opts := preparedTaskOptions(t, f, ag, *overrides.Budget)
	contract, err := f.storage.GetContract(f.ctx, f.request.Job.ContractID)
	require.NoError(t, err)
	taskID = opts.Claim.TaskID
	p, err = planning.Seal(planning.Plan{SchemaVersion: 1, ID: opts.Claim.PlanID, Scope: cortex.ScopeFromContext(f.ctx), Revision: 1, Objective: contract.Objective, Contract: contract, Budget: *overrides.Budget, Owners: []string{opts.Claim.Worker.Agent}, Tasks: []planning.Task{{ID: taskID, Instruction: "task", Owner: opts.Claim.Worker.Agent, Criteria: []string{contract.Criteria[0].ID}, Budget: *overrides.Budget, Output: planning.ArtifactRequirement{Name: "answer", MediaType: "text/plain", MaxBytes: 4096}}}, Actor: "host", Reason: "create"}, planning.Limits{})
	require.NoError(t, err)
	require.NoError(t, f.storage.CreatePlan(f.ctx, p))
	allocation := execution.Resources{Steps: 3, Tokens: 1000, ToolCalls: 1, FanOut: 1}
	require.NoError(t, f.storage.CreateReservationPolicy(f.ctx, execution.ReservationPolicy{SchemaVersion: 1, PlanID: p.ID, Scope: p.Scope, Limits: allocation, Deadline: p.Budget.Deadline, TokenOnly: true}))
	ready, err := f.storage.ClaimReadyTask(f.ctx, coordination.ReadyTaskClaimRequest{SchemaVersion: 1, Acquisition: coordination.BudgetedClaimRequest{Claim: coordination.ClaimRequest{SchemaVersion: 1, ID: opts.Claim.ClaimID, PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: taskID, Worker: opts.Claim.Worker, GrantRef: "grant", Duration: time.Minute}, Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), PlanID: p.ID, PlanDigest: p.Digest, Scope: p.Scope, TaskID: taskID, OwnerRef: opts.Claim.ClaimID.String(), Kind: execution.ReservationLeaf, Allocation: allocation, Deadline: p.Budget.Deadline}}})
	require.NoError(t, err)
	h := opts.Assignment.Handoff
	h.PlanDigest, h.Criteria = p.Digest, p.Tasks[0].Criteria
	h, err = coordination.SealHandoff(h)
	require.NoError(t, err)
	parent := &run.Run{ID: h.ParentRunID, AgentID: ag.ID, State: run.StateRunning}
	require.NoError(t, f.storage.CreateRun(f.ctx, parent))
	assigned, err := f.storage.ProposeAssignment(f.ctx, h)
	require.NoError(t, err)
	assigned, err = f.storage.AdvanceAssignment(f.ctx, coordination.AssignmentChange{AssignmentID: h.AssignmentID, ExpectedRevision: assigned.Revision, HandoffDigest: h.Digest, Fence: h.Fence, State: coordination.AssignmentAccepted, Actor: h.Receiver.Principal, Reason: "accept"})
	require.NoError(t, err)
	dispatched, err := f.storage.DispatchReadyTask(f.ctx, coordination.TaskDispatchRequest{SchemaVersion: 1, Scope: p.Scope, Claim: ready.Acquisition.Claim.Token(), ClaimDigest: ready.Acquisition.Claim.Digest, AdmissionDigest: ready.RequestDigest, AssignmentID: h.AssignmentID, AssignmentRevision: assigned.Revision, AssignmentDigest: assigned.Digest, ParentRunRevision: parent.Revision, RunID: opts.RunID, Actor: h.Receiver.Principal, Reason: "dispatch"})
	require.NoError(t, err)
	opts.Claim, opts.DispatchDigest, opts.Assignment = dispatched.Claim.Token(), dispatched.RequestDigest, dispatched.Assignment
	prepared, err := e.PrepareAgent(f.ctx, ag.ID, overrides)
	require.NoError(t, err)
	req, err := e.PrepareTaskRun(f.ctx, prepared, "task", opts)
	require.NoError(t, err)
	admitter, err := coordination.NewTaskRunAdmitter(f.storage, preparedTaskFence(func(_ context.Context, r coordination.TaskRunRequest, next func(coordination.TaskRunRequest) error) error {
		return next(r)
	}), preparedTaskAuthorizer(func(context.Context, coordination.TaskRunRequest) error { return nil }))
	require.NoError(t, err)
	return f, e, client, prepared, req, admitter
}

func TestPrepareTaskRunCreatesReservedChildThroughAdmitter(t *testing.T) {
	f, _, client, _, req, admitter := preparedTaskRuntimeFixture(t)
	result, err := admitter.Admit(f.ctx, req)
	require.NoError(t, err)
	require.True(t, result.Fresh)
	require.NoError(t, result.Validate(req))
	saved, err := f.storage.GetRun(f.ctx, req.Run.ID)
	require.NoError(t, err)
	require.Equal(t, req.Run.Provenance, saved.Provenance)
	intent, err := f.storage.GetRunCaptureIntent(f.ctx, req.Run.ID)
	require.NoError(t, err)
	require.Equal(t, req.Intent, intent)
	replay, err := admitter.Admit(f.ctx, req)
	require.NoError(t, err)
	require.False(t, replay.Fresh)
	require.Zero(t, client.calls)
	require.Zero(t, f.adapter.evaluations)
}

func TestPrepareTaskRunAllowsTaskContractProjection(t *testing.T) {
	criterion := verification.Criterion{ID: "first", Kind: verification.KindTest, Required: true, Verifier: "tests", VerifierRevision: "1", Parameters: verification.Parameters{ManifestRevision: "1"}, MaxAge: time.Minute}
	second := criterion
	second.ID = "second"
	f := newWorkerFixture(t, criterion, second)
	contract, err := f.storage.GetContract(f.ctx, f.request.Job.ContractID)
	require.NoError(t, err)
	budget := execution.Budget{MaxSteps: 3, MaxTokens: 1000, MaxToolCalls: 1, Deadline: time.Now().Add(2 * time.Minute)}
	firstID, secondID := id.New(planning.PrefixTask), id.New(planning.PrefixTask)
	plan, err := planning.Seal(planning.Plan{SchemaVersion: 1, ID: id.New(planning.PrefixPlan), Scope: contract.Scope, Revision: 1, Objective: contract.Objective, Contract: contract, Budget: budget, Owners: []string{"worker"}, Tasks: []planning.Task{
		{ID: firstID, Instruction: "first task", Owner: "worker", Criteria: []string{"first"}, Budget: budget, Output: planning.ArtifactRequirement{Name: "first", MediaType: "text/plain", MaxBytes: 4096}},
		{ID: secondID, Instruction: "second task", Owner: "worker", Criteria: []string{"second"}, Budget: budget, Output: planning.ArtifactRequirement{Name: "second", MediaType: "text/plain", MaxBytes: 4096}},
	}, Actor: "host", Reason: "project checks"}, planning.Limits{})
	require.NoError(t, err)
	projected, err := planning.TaskContract(plan, firstID, id.New(verification.PrefixContract))
	require.NoError(t, err)
	require.NoError(t, planning.ValidateTaskContract(plan, firstID, projected))
	require.NoError(t, f.storage.CreateContract(f.ctx, projected))
	e, ag, overrides := requiredSetup(t, f, &requiredClient{}, func(c *engine.RequiredVerificationConfig) {
		c.ContractID = projected.ID
		base := c.Builder
		c.Builder = requiredBuilder(func(ctx context.Context, in engine.VerificationIntentInput) (verification.CaptureIntent, error) {
			intent, buildErr := base.BuildVerificationIntent(ctx, in)
			if buildErr != nil {
				return intent, buildErr
			}
			intent.Template.ContractID = projected.ID
			intent.Template.Binding.ContractDigest = projected.Digest
			return verification.SealCaptureIntent(intent)
		})
	})
	overrides.Budget.MaxToolCalls = 1
	opts := preparedTaskOptions(t, f, ag, *overrides.Budget)
	p, err := e.PrepareAgent(f.ctx, ag.ID, overrides)
	require.NoError(t, err)
	req, err := e.PrepareTaskRun(f.ctx, p, "task", opts)
	require.NoError(t, err)
	require.Equal(t, projected.ID, req.Intent.Template.ContractID)
	require.Equal(t, projected.Digest, req.Intent.Template.Binding.ContractDigest)
	require.NotEqual(t, opts.Assignment.Handoff.ContractDigest, projected.Digest)
}
