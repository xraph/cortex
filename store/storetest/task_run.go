package storetest

import (
	"context"
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
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func taskRunFixture(t *testing.T, backend store.Store, driver assurance.Driver) (context.Context, coordination.TaskRunRequest, *run.Run) {
	t.Helper()
	ctx, admission := readyClaimFixture(t, backend)
	agentID := mustCreateAgent(t, backend, ctx, "")
	admission.Acquisition.Claim.Worker.Agent = agentID.String()
	repo := assurance.New(driver)
	request, parent := dispatchFixture(ctx, t, backend, repo, admission)
	return ctx, taskRunForDispatch(ctx, t, backend, repo, request, parent), parent
}

func taskRunForDispatch(ctx context.Context, t *testing.T, backend store.Store, repo *assurance.Repository, request coordination.TaskDispatchRequest, parent *run.Run) coordination.TaskRunRequest {
	t.Helper()
	agentID, err := id.Parse(request.Claim.Worker.Agent)
	require.NoError(t, err)
	dispatched, err := repo.DispatchReadyTask(ctx, request)
	require.NoError(t, err)
	p, err := repo.GetPlan(ctx, request.Claim.PlanID)
	require.NoError(t, err)
	require.NoError(t, repo.CreateContract(ctx, p.Contract))
	r := run.Run{ID: request.RunID, AgentID: agentID, SessionID: mustCreateSession(t, backend, ctx, agentID, "task"), Scope: request.Scope, State: run.StateRunning, Input: "scanned task", Budget: execution.Budget{MaxSteps: 1, MaxTokens: 600, MaxToolCalls: 1, Deadline: p.Budget.Deadline}, Provenance: run.ExecutionProvenance{SchemaVersion: 1, Purpose: run.PurposeNormal, PreparedDigest: evidence.Digest([]byte("prepared")), ModelRevision: "model:1", Tools: []run.Revision{{Name: "lookup", Revision: "1"}}, AssignmentDigest: dispatched.Assignment.Handoff.Digest, ParentRunID: parent.ID.String(), AcceptanceRef: p.Contract.ID.String(), VerificationRequired: true}}
	inputs := verification.ExecutionInputs{SchemaVersion: 1, RunID: r.ID, AgentID: agentID, SessionID: r.SessionID, Input: r.Input, PreparedDigest: r.Provenance.PreparedDigest, Model: "model", ModelRevision: r.Provenance.ModelRevision, Tools: []verification.ToolRevision{{Name: "lookup", Revision: "1"}}}
	data, err := inputs.Encode()
	require.NoError(t, err)
	candidate := evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: r.Scope, Version: "1", Producer: planning.ArtifactProducer(p, request.Claim.TaskID), MediaType: "text/plain", Sensitivity: evidence.SensitivityRestricted, SHA256: evidence.Digest(nil), CreatedAt: time.Now().UTC().Add(-time.Second), RetainUntil: p.Budget.Deadline.Add(time.Hour)}
	inputRef := candidate
	inputRef.ID = id.New(evidence.PrefixArtifact)
	inputRef.MediaType = "application/json"
	artifact, err := evidence.Seal(evidence.Artifact{Ref: inputRef, Data: data})
	require.NoError(t, err)
	require.NoError(t, repo.SaveEvidence(ctx, artifact))
	transcript := candidate
	transcript.ID = id.New(evidence.PrefixArtifact)
	transcript.MediaType = "application/json"
	binding := verification.Binding{Scope: r.Scope, RunID: r.ID, ContractDigest: p.Contract.Digest, Candidate: candidate, Evidence: []evidence.Ref{artifact.Ref}, Context: verification.ContextSnapshot{PrincipalRef: request.Claim.Worker.Principal, AuthorityRevision: "1", PreparedDigest: r.Provenance.PreparedDigest, PolicyRevision: "1", EnvironmentRevision: "1", DatasetRevision: "1", Sources: p.Sources, Tools: inputs.Tools, ExecutionInputs: &artifact.Ref}}
	intent, err := verification.SealCaptureIntent(verification.CaptureIntent{SchemaVersion: 2, ID: id.New(verification.PrefixCaptureIntent), Template: verification.JobRequest{SchemaVersion: 1, ID: id.New(verification.PrefixJob), ExpectedRunRevision: 1, ContractID: p.Contract.ID, Binding: binding, Deadline: r.Budget.Deadline, MaxAttempts: 2, EvaluationBudget: &verification.EvaluationBudget{MaxCalls: 2, MaxTokens: 100, TokenOnly: true}}, ResultID: id.New(verification.PrefixResult), MaxCandidateBytes: p.Tasks[0].Output.MaxBytes, Transcript: &transcript, MaxTranscriptBytes: 1 << 20})
	require.NoError(t, err)
	r.Provenance.VerificationIntentID, r.Provenance.ExecutionInputsDigest = intent.ID.String(), artifact.Ref.SHA256
	return coordination.TaskRunRequest{SchemaVersion: 1, Claim: dispatched.Claim.Token(), DispatchDigest: dispatched.RequestDigest, AssignmentRevision: dispatched.Assignment.Revision, AssignmentDigest: dispatched.Assignment.Digest, ParentRunRevision: parent.Revision, Dependencies: request.Dependencies, Run: r, Intent: intent, Actor: request.Actor, Reason: "start child"}
}

// TaskRunTransactions tests atomic reserved child creation and capture policy.
func TaskRunTransactions(t *testing.T, backend store.Store, driver assurance.Driver) {
	plans, ok := driver.(assurance.PlanDriver)
	require.True(t, ok)
	fixture := func(t *testing.T) (context.Context, *assurance.Repository, coordination.TaskRunRequest, *run.Run) {
		ctx, req, parent := taskRunFixture(t, backend, driver)
		return ctx, assurance.New(driver), req, parent
	}
	t.Run("create_retry_scope", func(t *testing.T) {
		ctx, repo, req, parent := fixture(t)
		require.NotNil(t, resultCapability[coordination.TaskRunStore](t, backend))
		result, err := repo.CreateTaskRun(ctx, req)
		require.NoError(t, err)
		require.True(t, result.Fresh)
		require.NoError(t, result.Validate(req))
		actual, err := backend.GetRun(ctx, req.Run.ID)
		require.NoError(t, err)
		initial, buildErr := req.InitialRun(result.CreatedAt)
		require.NoError(t, buildErr)
		require.True(t, initial.CreatedAt.Equal(actual.CreatedAt))
		require.True(t, initial.UpdatedAt.Equal(actual.UpdatedAt))
		require.NotNil(t, actual.StartedAt)
		require.True(t, initial.StartedAt.Equal(*actual.StartedAt))
		// PostgreSQL returns timestamp columns in the connection's time zone.
		canonical := *actual
		canonical.CreatedAt = canonical.CreatedAt.UTC()
		canonical.UpdatedAt = canonical.UpdatedAt.UTC()
		startedAt := canonical.StartedAt.UTC()
		canonical.StartedAt = &startedAt
		require.Equal(t, initial, canonical)
		rows, readErr := driver.Select(ctx, assurance.Query{Kind: "task_run_start", ID: req.Claim.ClaimID.String(), Limit: 1})
		require.NoError(t, readErr)
		require.Len(t, rows, 1)
		require.NotContains(t, rows[0].Body, req.Run.Input, "creation metadata must not duplicate private input")

		require.EqualValues(t, 1, actual.Revision)
		intent, err := repo.GetRunCaptureIntent(ctx, req.Run.ID)
		require.NoError(t, err)
		require.Equal(t, req.Intent, intent)
		parent.State = run.StateCancelled
		require.NoError(t, backend.UpdateRun(ctx, parent))
		actual.State = run.StateCancelled
		require.NoError(t, backend.UpdateRun(ctx, actual))
		require.NoError(t, repo.TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), *req.Intent.Template.Binding.Context.ExecutionInputs, "deleted"))
		retry, err := repo.CreateTaskRun(ctx, req)
		require.NoError(t, err)
		result.Fresh = false
		require.Equal(t, result, retry)
		loaded, err := repo.GetTaskRunStart(ctx, req.Claim.ClaimID)
		require.NoError(t, err)
		require.Equal(t, result, loaded)
		current, err := backend.GetRun(ctx, req.Run.ID)
		require.NoError(t, err)
		require.Equal(t, run.StateCancelled, current.State)
		req.Run.Input = "changed"
		_, err = repo.CreateTaskRun(ctx, req)
		require.Error(t, err)
		_, err = repo.GetTaskRunStart(context.Background(), req.Claim.ClaimID)
		require.ErrorIs(t, err, cortex.ErrNoScope)
		_, err = repo.GetTaskRunStart(ctxWithScope("other"), req.Claim.ClaimID)
		require.ErrorIs(t, err, coordination.ErrClaimNotFound)
	})
	for _, kind := range []string{"task_child_run", "capture_intent", "capture_intent_run", "capture_intent_job", "capture_intent_result", "task_run_start"} {
		t.Run("rollback_"+kind, func(t *testing.T) {
			ctx, repo, req, _ := fixture(t)
			faulty := assurance.New(&claimWriteDriver{Driver: driver, plans: plans, failKind: kind})
			result, err := faulty.CreateTaskRun(ctx, req)
			require.ErrorIs(t, err, errAssignmentInjected)
			require.Zero(t, result)
			_, err = backend.GetRun(ctx, req.Run.ID)
			require.ErrorIs(t, err, cortex.ErrRunNotFound)
			_, err = repo.GetRunCaptureIntent(ctx, req.Run.ID)
			require.ErrorIs(t, err, verification.ErrNotFound)
			_, err = repo.GetTaskRunStart(ctx, req.Claim.ClaimID)
			require.ErrorIs(t, err, coordination.ErrClaimNotFound)
			_, err = repo.CreateTaskRun(ctx, req)
			require.NoError(t, err)
		})
	}
	t.Run("lost_acknowledgment", func(t *testing.T) {
		ctx, repo, req, _ := fixture(t)
		result, err := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, lostAck: true}).CreateTaskRun(ctx, req)
		require.ErrorIs(t, err, errAssignmentInjected)
		require.Zero(t, result)
		retry, err := repo.CreateTaskRun(ctx, req)
		require.NoError(t, err)
		require.False(t, retry.Fresh)
	})
	for _, mode := range []string{"existing_child", "cancelled_parent", "parent_revision", "cancelled_assignment", "claim_revision", "dispatch_digest", "assignment_digest", "replan", "expired", "expiry_after_write", "input_deleted", "wrong_input", "wrong_principal", "wrong_handoff", "wrong_parent", "budget", "unlimited_budget", "wrong_run", "output_limit", "legacy_intent", "wrong_scope", "cancelled_context"} {
		t.Run(mode, func(t *testing.T) {
			ctx, repo, req, parent := fixture(t)
			switch mode {
			case "existing_child":
				existing := req.Run
				require.NoError(t, backend.CreateRun(ctx, &existing))
			case "cancelled_parent":
				parent.State = run.StateCancelled
				require.NoError(t, backend.UpdateRun(ctx, parent))
				req.ParentRunRevision = parent.Revision
			case "parent_revision":
				req.ParentRunRevision++
			case "cancelled_assignment":
				a, err := repo.GetTaskDispatch(ctx, req.Claim.ClaimID)
				require.NoError(t, err)
				_, err = repo.AdvanceAssignment(ctx, assignmentChange(a.Assignment, coordination.AssignmentCancelled))
				require.NoError(t, err)
			case "claim_revision":
				req.Claim.Revision++
			case "dispatch_digest":
				req.DispatchDigest = evidence.Digest([]byte("different"))
			case "assignment_digest":
				req.AssignmentDigest = evidence.Digest([]byte("different"))
			case "replan":
				p, err := repo.GetPlan(ctx, req.Claim.PlanID)
				require.NoError(t, err)
				_, err = repo.CommitRevision(ctx, planning.Revision{Plan: p, Actor: "host", Reason: "new"}, p.Revision)
				require.NoError(t, err)
			case "expired":
				repo = repo.WithClock(&jobClock{at: time.Now().UTC().Add(2 * time.Minute)})
			case "expiry_after_write":
				clock := &jobClock{at: time.Now().UTC()}
				repo = assurance.New(&claimWriteDriver{Driver: driver, plans: plans, clock: clock, advanceKind: "task_run_start"}).WithClock(clock)
			case "input_deleted":
				require.NoError(t, repo.TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), *req.Intent.Template.Binding.Context.ExecutionInputs, "deleted"))
			case "wrong_input":
				req.Run.Input = "substitution"
			case "wrong_principal":
				req.Intent.Template.Binding.Context.PrincipalRef = "other"
			case "wrong_handoff":
				req.Run.Provenance.AssignmentDigest = evidence.Digest([]byte("other"))
			case "wrong_parent":
				req.Run.Provenance.ParentRunID = id.NewAgentRunID().String()
			case "budget":
				req.Run.Budget.MaxTokens++
			case "unlimited_budget":
				req.Run.Budget.MaxTokens = 0
			case "wrong_run":
				req.Run.ID = id.NewAgentRunID()
			case "output_limit":
				req.Intent.MaxCandidateBytes++
			case "legacy_intent":
				req.Intent.SchemaVersion = 1
				req.Intent.Transcript = nil
				req.Intent.MaxTranscriptBytes = 0
			case "wrong_scope":
				ctx = ctxWithScope("other")
			case "cancelled_context":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			var err error
			req.Intent, err = verification.SealCaptureIntent(req.Intent)
			require.NoError(t, err)
			result, err := repo.CreateTaskRun(ctx, req)
			require.Error(t, err)
			require.Zero(t, result)
		})
	}
	for _, mode := range []string{"valid", "missing", "changed_context", "deleted", "expired_after_write"} {
		t.Run("dependency_"+mode, func(t *testing.T) {
			ctx, root := taskAcceptanceFixture(t, backend)
			repo := assurance.New(driver)
			accepted, err := repo.AcceptTaskResult(ctx, root)
			require.NoError(t, err)
			p, err := repo.GetPlan(ctx, root.Claim.PlanID)
			require.NoError(t, err)
			policy := execution.ReservationPolicy{SchemaVersion: 1, PlanID: p.ID, Scope: p.Scope, Limits: execution.Resources{Steps: 10, Tokens: 1000, ToolCalls: 10, FanOut: 10}, Deadline: p.Budget.Deadline, TokenOnly: true}
			require.NoError(t, repo.CreateReservationPolicy(ctx, policy))
			claim := coordination.ClaimRequest{SchemaVersion: 1, ID: id.New(coordination.PrefixClaim), PlanID: p.ID, PlanRevision: p.Revision, PlanDigest: p.Digest, TaskID: p.Tasks[1].ID, Worker: coordination.Identity{Agent: mustCreateAgent(t, backend, ctx, "").String(), Principal: "worker"}, GrantRef: "grant", Duration: 5 * time.Minute}
			admission := coordination.ReadyTaskClaimRequest{SchemaVersion: 1, Acquisition: coordination.BudgetedClaimRequest{Claim: claim, Reservation: execution.ReservationRequest{SchemaVersion: 1, ID: id.New(execution.PrefixReservation), Scope: p.Scope, PlanID: p.ID, PlanDigest: p.Digest, TaskID: claim.TaskID, OwnerRef: claim.ID.String(), Kind: execution.ReservationLeaf, Allocation: execution.Resources{Steps: 1, Tokens: 600, ToolCalls: 1, FanOut: 1}, Deadline: p.Budget.Deadline}}, Dependencies: []coordination.AcceptedDependency{{AcceptanceID: root.ID, Digest: accepted.Digest, Current: root.Current}}}
			dispatch, parent := dispatchFixture(ctx, t, backend, repo, admission)
			req := taskRunForDispatch(ctx, t, backend, repo, dispatch, parent)
			switch mode {
			case "missing":
				req.Dependencies = nil
			case "changed_context":
				req.Dependencies[0].Current.Context.AuthorityRevision = "changed"
			case "deleted":
				require.NoError(t, repo.TombstoneEvidence(evidence.WithDeletionActor(ctx, "host"), root.Current.Candidate, "deleted"))
			case "expired_after_write":
				clock := &jobClock{at: time.Now().UTC().Add(90 * time.Second)}
				repo = assurance.New(&claimWriteDriver{Driver: driver, plans: plans, clock: clock, advanceKind: "task_run_start"}).WithClock(clock)
			}
			result, err := repo.CreateTaskRun(ctx, req)
			if mode == "valid" {
				require.NoError(t, err)
				require.True(t, result.Fresh)
			} else {
				require.Error(t, err)
				require.Zero(t, result)
				_, err = backend.GetRun(ctx, req.Run.ID)
				require.ErrorIs(t, err, cortex.ErrRunNotFound)
			}
		})
	}
	for _, identical := range []bool{false, true} {
		name := "distinct"
		if identical {
			name = "identical"
		}
		t.Run("concurrent_"+name, func(t *testing.T) {
			ctx, repo, req, _ := fixture(t)
			var wg sync.WaitGroup
			results := make(chan coordination.TaskRunStart, 2)
			failures := make(chan error, 2)
			for i := 0; i < 2; i++ {
				next, _, err := req.Freeze()
				require.NoError(t, err)
				if i > 0 && !identical {
					next.Reason = "different attempt"
				}
				wg.Go(func() { value, err := repo.CreateTaskRun(ctx, next); results <- value; failures <- err })
			}
			wg.Wait()
			close(results)
			close(failures)
			fresh, success := 0, 0
			for value := range results {
				if value.Fresh {
					fresh++
				}
			}
			for err := range failures {
				if err == nil {
					success++
				} else {
					require.ErrorIs(t, err, coordination.ErrClaimConflict)
				}
			}
			require.Equal(t, 1, fresh)
			expected := 1
			if identical {
				expected = 2
			}
			require.Equal(t, expected, success)
		})
	}
}
