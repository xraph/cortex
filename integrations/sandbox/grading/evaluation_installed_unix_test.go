//go:build linux || darwin

package grading

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/verification"
)

type gradingWallClock struct{}

func (gradingWallClock) Now() time.Time { return time.Now().UTC() }

type gradingEvidenceAuthority struct{}

func (gradingEvidenceAuthority) AuthorizeEvidence(context.Context, evidence.Action, evidence.Ref) error {
	return nil
}

func TestInstalledEvaluationSettlesExistingExecutorAndRetainsObservationBindings(t *testing.T) {
	cfg, d, runner, authority, memory := gradingFixture(t)
	ctx := cortex.WithScope(t.Context(), d.Input.Request.Binding.Scope)
	drv := sqlitedriver.New()
	require.NoError(t, drv.Open(ctx, filepath.Join(t.TempDir(), "grading.db")+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"))
	db, err := grove.Open(drv)
	require.NoError(t, err)
	storage := sqlite.New(db)
	require.NoError(t, storage.Migrate(ctx))
	defer storage.Close()
	for _, artifact := range memory.data {
		require.NoError(t, storage.SaveEvidence(ctx, artifact))
	}
	repo, err := evidence.NewRepository(storage, gradingEvidenceAuthority{}, 1<<20)
	require.NoError(t, err)
	runner.repo = repo
	cfg.Tests.Repository = repo
	cfg.Tests.Clock = gradingWallClock{}
	original := cfg.Tests.Supervisor.(*EqualitySupervisor)
	registrations := make([]SupervisorRegistration, 0, len(original.registrations))
	for _, registration := range original.registrations {
		registrations = append(registrations, registration)
	}
	cfg.Tests.Supervisor, err = NewEqualitySupervisor(SupervisorConfig{Registrations: registrations, Repository: repo, Authorizer: authority, Clock: gradingWallClock{}})
	require.NoError(t, err)
	adapter, err := NewEvaluationAdapter(cfg)
	require.NoError(t, err)
	require.NoError(t, adapter.Start(t.Context()))
	defer adapter.Stop(context.Background())
	a := &agent.Config{ID: id.NewAgentID(), Name: "grading-worker"}
	require.NoError(t, storage.Create(ctx, a))
	source := &run.Run{ID: d.Input.Request.Binding.RunID, AgentID: a.ID, State: run.StateCompleted, StopReason: run.StopFinalAnswer, VerificationStatus: verification.StatusUnchecked, Input: "captured candidate", Output: string(memory.data[d.Input.Request.Binding.Candidate.ID].Data), Provenance: run.ExecutionProvenance{PreparedDigest: d.Input.Request.Binding.Context.PreparedDigest, AcceptanceRef: d.Input.Request.Contract.ID.String()}}
	require.NoError(t, storage.CreateRun(ctx, source))
	require.NoError(t, storage.CreateContract(ctx, d.Input.Request.Contract))
	job, err := storage.EnqueueVerificationJob(ctx, verification.JobRequest{SchemaVersion: 1, ID: d.Input.JobID, ExpectedRunRevision: source.Revision, ContractID: d.Input.Request.Contract.ID, Binding: d.Input.Request.Binding, Deadline: d.Input.Request.Deadline, MaxAttempts: 3, EvaluationBudget: &d.Input.Budget})
	require.NoError(t, err)
	job, err = storage.ClaimVerificationJob(ctx, verification.JobClaim{JobID: job.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker", Duration: 30 * time.Second})
	require.NoError(t, err)
	executor, err := verification.NewExecutor(verification.ExecutorConfig{Store: storage, Adapter: adapter, Authorizer: authority, Clock: gradingWallClock{}})
	require.NoError(t, err)
	selection := verification.ExecutorRequest{Lease: *job.Lease, Verifier: d.Input.Request.Verifier, VerifierRevision: d.Input.Request.VerifierRevision}
	_, err = executor.Execute(ctx, selection)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	require.Eventually(t, func() bool {
		var executeErr error
		job, executeErr = executor.Execute(ctx, selection)
		return executeErr == nil && job.State == verification.JobReady
	}, 3*time.Second, 10*time.Millisecond)
	require.Len(t, job.Calls, 1)
	require.Equal(t, int64(100), verification.DebitCalls(job.Calls).USDMicros)
	require.Equal(t, 2, runner.count())
	receipt, err := storage.CommitVerifiedResult(ctx, verification.CommitRequest{SchemaVersion: 1, ResultID: id.New(verification.PrefixResult), ExpectedRunRevision: job.RunRevision, ContractID: job.Request.ContractID, ReportIDs: job.ReportIDs, Current: job.Request.Binding, JobLease: job.Lease})
	require.NoError(t, err)
	read := verification.ResultReadRequest{SchemaVersion: 1, RunID: source.ID, ResultID: receipt.Request.ResultID, ReceiptDigest: receipt.Digest}
	snapshot, err := storage.ReadResultSnapshot(ctx, read)
	require.NoError(t, err)
	refs := snapshot.Reports[0].Outcomes[0].ObservationRefs
	require.Len(t, refs, 3)
	require.NoError(t, storage.TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), refs[2], "supervisor observation revoked"))
	snapshot, err = storage.ReadResultSnapshot(ctx, read)
	require.ErrorIs(t, err, evidence.ErrDeleted)
	require.Empty(t, snapshot.Output)
}
