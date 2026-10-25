package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

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

type taskRecoveryFixture struct {
	ctx     context.Context
	repo    *assurance.Repository
	clock   *jobClock
	request coordination.TaskAcceptanceRequest
	model   execution.RunCallRequest
	effect  execution.EffectRequest
}

type taskRecoveryOptions struct {
	Acquire        func(context.Context, coordination.ClaimRequest, planning.Plan) (coordination.Claim, error)
	ProviderUsage  execution.ReservationUsage
	VerifierBudget *verification.EvaluationBudget
	Ordinary       bool
}

func newTaskRecoveryFixture(t *testing.T, backend store.Store, driver assurance.Driver, mode string, options ...taskRecoveryOptions) taskRecoveryFixture {
	t.Helper()
	var option taskRecoveryOptions
	if len(options) > 0 {
		option = options[0]
	}
	config := taskContractConfig(t)
	ctx, p, h := assignmentFixture(t, backend, config.Plan)
	worker, claimID := mustCreateAgent(t, backend, ctx, "recovery-worker"), id.New(coordination.PrefixClaim)
	h.Receiver.Agent = worker.String()
	var err error
	h, err = coordination.SealHandoff(h)
	require.NoError(t, err)
	contract := config.Contract(p)
	_, working, change, job := submissionFixtureForHandoffBudget(ctx, t, backend, p, h, contract, option.VerifierBudget, func(r *run.Run, _ *verification.Binding) {
		r.AgentID = worker
		r.Provenance.TaskClaimID = claimID.String()
	})
	claimOption := acceptanceClaimFixture{ID: claimID, Duration: 10 * time.Second}
	if option.Acquire != nil {
		claimOption.Acquire = func(ctx context.Context, req coordination.ClaimRequest) (coordination.Claim, error) {
			return option.Acquire(ctx, req, p)
		}
	}
	req := finishTaskAcceptanceFixture(ctx, t, backend, working, change, job, claimOption)
	repo := assurance.New(driver)
	claim, err := repo.GetTaskClaim(ctx, req.Claim.PlanID, req.Claim.TaskID)
	require.NoError(t, err)
	model := storedRunCall()
	model.Scope = h.Scope
	model.RunID = req.Result.RunID
	model.AgentID = worker
	model.ClaimID = claimID
	model.ParentRunID = h.ParentRunID
	model.PreparedDigest = req.Current.Context.PreparedDigest
	model.AssignmentDigest = h.Digest
	model.Deadline = claim.ExpiresAt
	switch mode {
	case "foreign claim":
		model.ClaimID = id.New(coordination.PrefixClaim)
	case "foreign worker":
		model.AgentID = id.NewAgentID()
	case "foreign parent":
		model.ParentRunID = id.NewAgentRunID()
	case "foreign prepared":
		model.PreparedDigest = evidence.Digest([]byte("other prepared"))
	case "foreign assignment":
		model.AssignmentDigest = evidence.Digest([]byte("other assignment"))
	}

	_, err = repo.BeginRunCall(ctx, model)
	require.NoError(t, err)
	if mode != "missing provider" {
		proof := snapshotProof(t, model)
		proof.Usage = option.ProviderUsage
		if mode == "future provider" {
			proof.ObservedAt = claim.ExpiresAt.Add(2 * time.Second)
		}
		require.NoError(t, repo.PutProviderCallReceipt(ctx, model, proof))
	}
	if mode == "contradictory return" {
		_, digest, freezeErr := model.Freeze()
		require.NoError(t, freezeErr)
		require.NoError(t, repo.CompleteRunCall(ctx, model, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: evidence.Digest([]byte("different result"))}))
	}
	tool := model
	tool.ID = id.New(execution.PrefixRunCall)
	tool.Kind = execution.RunCallTool
	tool.OperationDigest = evidence.Digest([]byte("tool:1"))
	_, err = repo.BeginRunCall(ctx, tool)
	require.NoError(t, err)
	effect := storedEffect()
	effect.Scope = h.Scope
	effect.RunID = model.RunID
	effect.Deadline = claim.ExpiresAt
	effect.RunCall = &tool
	if mode == "legacy effect" {
		effect.RunCall = nil
	}
	_, err = repo.BeginEffect(ctx, effect)
	require.NoError(t, err)
	if mode != "pending effect" {
		require.NoError(t, repo.CompleteEffect(ctx, effect, storedEffectReceipt(t, effect)))
	}
	if option.Ordinary {
		clock := &jobClock{at: time.Now().UTC()}
		return taskRecoveryFixture{ctx: ctx, repo: repo.WithClock(clock), clock: clock, request: req, model: model, effect: effect}
	}
	clock := &jobClock{at: claim.ExpiresAt.Add(time.Second)}
	repo = repo.WithClock(clock)
	recovering, err := repo.ExpireTaskClaim(ctx, claim.PlanID, claim.TaskID, claim.Revision)
	require.NoError(t, err)
	req.Claim = recovering.Token()
	seal := snapshotSeal(ctx, t, repo, model)
	snapshot, err := repo.SnapshotRunRecovery(ctx, seal)
	require.NoError(t, err)
	req.Recovery = &coordination.TaskRecoveryProof{SchemaVersion: 1, ClaimDigest: recovering.Digest, Seal: seal, SnapshotDigest: snapshot.Digest, Issuer: "fixture", Revision: "1", ProofRef: "stopped-worker-incarnation", ProofDigest: evidence.Digest([]byte("quiescence proof")), ObservedAt: clock.at, ExpiresAt: clock.at.Add(30 * time.Second)}
	return taskRecoveryFixture{ctx: ctx, repo: repo, clock: clock, request: req, model: model, effect: effect}
}

// TaskRecoveryAdoption exercises recovery against real durable verifier and
// ownership records. Fixture proof references do not qualify a host authenticator.
func TaskRecoveryAdoption(t *testing.T, backend store.Store, driver assurance.Driver) {
	t.Helper()
	t.Run("accepted task call seal", func(t *testing.T) { testTaskCallSeal(t, backend, driver) })
	t.Run("guarded usage settlement", func(t *testing.T) { testTaskUsageSettlement(t, backend, driver) })
	t.Run("historical usage settlement", func(t *testing.T) { testTaskSettlementHistory(t, backend, driver) })
	t.Run("usage admission closure", func(t *testing.T) { testTaskUsageClosure(t, backend, driver) })
	t.Run("complete usage snapshot", func(t *testing.T) { testTaskUsageSnapshot(t, backend, driver) })
	t.Run("engine managed adoption", func(t *testing.T) { testManagedTaskAdoption(t, backend, driver) })
	t.Run("engine managed settlement", func(t *testing.T) { testManagedTaskSettlement(t, backend, driver) })
	t.Run("adopts without renewing or reinvoking", func(t *testing.T) {
		f := newTaskRecoveryFixture(t, backend, driver, "")
		before, err := backend.GetRun(f.ctx, f.request.Result.RunID)
		require.NoError(t, err)
		_, err = f.repo.AcceptTaskResult(f.ctx, f.request)
		require.Error(t, err, "ordinary acceptance cannot bypass recovery authority")
		got, err := f.repo.AdoptTaskResult(f.ctx, f.request)
		require.NoError(t, err)
		require.True(t, got.Fresh)
		require.NoError(t, got.Validate())
		require.NotEmpty(t, got.Claim.RecoveryDigest)
		require.True(t, !got.ObservedAt.Before(got.Claim.ExpiresAt))
		current, err := f.repo.GetTaskClaim(f.ctx, f.request.Claim.PlanID, f.request.Claim.TaskID)
		require.NoError(t, err)
		require.Equal(t, got.Claim, current)
		after, err := backend.GetRun(f.ctx, before.ID)
		require.NoError(t, err)
		require.Equal(t, before, after)
		model, err := f.repo.GetRunCall(f.ctx, f.model.RunID, f.model.OperationDigest)
		require.NoError(t, err)
		require.Nil(t, model.Completion)
		f.clock.advance(time.Hour)
		replay, err := f.repo.AdoptTaskResult(f.ctx, f.request)
		require.NoError(t, err)
		got.Fresh = false
		require.Equal(t, got, replay)
		changed := f.request
		changed.ID = id.New(coordination.PrefixTaskAcceptance)
		_, err = f.repo.AdoptTaskResult(f.ctx, changed)
		require.Error(t, err)
	})
	for _, mode := range []string{"missing provider", "pending effect", "legacy effect", "foreign claim", "foreign worker", "foreign parent", "foreign prepared", "foreign assignment", "future provider", "contradictory return", "late return", "changed proof", "expired proof", "deleted evidence", "stale token", "missing proof"} {
		t.Run(mode, func(t *testing.T) {
			f := newTaskRecoveryFixture(t, backend, driver, mode)
			switch mode {
			case "late return":
				p := snapshotProof(t, f.model)
				require.NoError(t, f.repo.CompleteRunCall(f.ctx, f.model, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: p.RequestDigest, ResultDigest: p.ResultDigest}))
			case "changed proof":
				f.request.Recovery.ClaimDigest = evidence.Digest([]byte("different"))
			case "expired proof":
				f.clock.at = f.request.Recovery.ExpiresAt
			case "deleted evidence":
				require.NoError(t, f.repo.TombstoneEvidence(evidence.WithDeletionActor(f.ctx, "host"), f.request.Current.Candidate, "revoked"))
			case "stale token":
				f.request.Claim.Revision--
			case "missing proof":
				f.request.Recovery = nil
			}
			got, err := f.repo.AdoptTaskResult(f.ctx, f.request)
			require.Error(t, err)
			require.Empty(t, got)
			claim, err := f.repo.GetTaskClaim(f.ctx, f.request.Claim.PlanID, f.request.Claim.TaskID)
			require.NoError(t, err)
			require.Equal(t, coordination.ClaimRecovering, claim.State)
			_, err = f.repo.GetTaskAcceptance(f.ctx, f.request.ID)
			require.ErrorIs(t, err, coordination.ErrTaskAcceptanceNotFound)
		})
	}
	t.Run("transaction faults", func(t *testing.T) {
		plans, ok := driver.(assurance.PlanDriver)
		require.True(t, ok)
		for _, mode := range []string{"rollback", "lost acknowledgment", "proof expires at commit"} {
			t.Run(mode, func(t *testing.T) {
				f := newTaskRecoveryFixture(t, backend, driver, "")
				var faulty *assurance.Repository
				switch mode {
				case "rollback":
					faulty = assurance.New(&claimWriteDriver{Driver: driver, plans: plans, failKind: "task_acceptance"})
				case "lost acknowledgment":
					faulty = assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, lostAck: true})
				case "proof expires at commit":
					faulty = assurance.New(&claimWriteDriver{Driver: driver, plans: plans, clock: f.clock, advanceKind: "accepted_task"})
				}
				got, err := faulty.WithClock(f.clock).AdoptTaskResult(f.ctx, f.request)
				require.Error(t, err)
				require.Empty(t, got)
				claim, err := f.repo.GetTaskClaim(f.ctx, f.request.Claim.PlanID, f.request.Claim.TaskID)
				require.NoError(t, err)
				if mode == "lost acknowledgment" {
					require.Equal(t, coordination.ClaimCompleted, claim.State)
					retry, retryErr := f.repo.AdoptTaskResult(f.ctx, f.request)
					require.NoError(t, retryErr)
					require.False(t, retry.Fresh)
				} else {
					require.Equal(t, coordination.ClaimRecovering, claim.State)
					_, readErr := f.repo.GetTaskAcceptance(f.ctx, f.request.ID)
					require.ErrorIs(t, readErr, coordination.ErrTaskAcceptanceNotFound)
				}
			})
		}
	})
	t.Run("recovered prerequisite remains current", func(t *testing.T) {
		for _, changed := range []bool{false, true} {
			t.Run(map[bool]string{false: "unchanged after proof window", true: "late journal evidence"}[changed], func(t *testing.T) {
				f := newTaskRecoveryFixture(t, backend, driver, "")
				root, err := f.repo.AdoptTaskResult(f.ctx, f.request)
				require.NoError(t, err)
				child := dependentTaskAcceptanceFixture(f.ctx, t, backend, f.request, 1, []evidence.Ref{f.request.Current.Candidate})
				child.Dependencies = []coordination.AcceptedDependency{{AcceptanceID: root.Request.ID, Digest: root.Digest, Current: root.Request.Current}}
				f.clock.advance(31 * time.Second)
				if changed {
					proof := snapshotProof(t, f.model)
					require.NoError(t, f.repo.CompleteRunCall(f.ctx, f.model, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: proof.RequestDigest, ResultDigest: proof.ResultDigest}))
				}
				got, err := f.repo.AcceptTaskResult(f.ctx, child)
				if changed {
					require.Error(t, err)
					require.Empty(t, got)
				} else {
					require.NoError(t, err)
					require.True(t, got.Fresh)
				}
			})
		}
	})
	t.Run("late receipt waits through adoption", func(t *testing.T) {
		f := newTaskRecoveryFixture(t, backend, driver, "")
		plans, ok := driver.(assurance.PlanDriver)
		require.True(t, ok)
		ctx, cancel := context.WithTimeout(f.ctx, 15*time.Second)
		defer cancel()
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		defer once.Do(func() { close(release) })
		repo := assurance.New(&submissionFenceDriver{Driver: driver, plans: plans, entered: entered, release: release}).WithClock(f.clock)
		type result struct {
			acceptance coordination.TaskAcceptance
			err        error
		}
		done := make(chan result, 1)
		go func() { got, err := repo.AdoptTaskResult(ctx, f.request); done <- result{got, err} }()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("adoption did not reach evidence read")
		}
		proof := snapshotProof(t, f.model)
		late := make(chan error, 1)
		started := make(chan struct{})
		go func() {
			close(started)
			late <- f.repo.CompleteRunCall(ctx, f.model, execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: proof.RequestDigest, ResultDigest: proof.ResultDigest})
		}()
		<-started
		select {
		case err := <-late:
			t.Fatalf("late return escaped adoption guard: %v", err)
		case <-time.After(150 * time.Millisecond):
		}
		once.Do(func() { close(release) })
		select {
		case got := <-done:
			require.NoError(t, got.err)
			require.True(t, got.acceptance.Fresh)
		case <-ctx.Done():
			t.Fatal("adoption did not complete")
		}
		select {
		case err := <-late:
			require.NoError(t, err)
		case <-ctx.Done():
			t.Fatal("late receipt did not complete")
		}
	})
}

func lockRecoveryJournal(ctx context.Context, driver assurance.Driver, runID id.AgentRunID) error {
	locker, ok := driver.(assurance.RunCallLocker)
	if !ok {
		return execution.ErrRunCallUnavailable
	}
	return locker.LockRunCalls(ctx, runID)
}
