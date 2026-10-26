package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

const teamVerificationCrashBoundary = "CORTEX_TEST_TEAM_VERIFICATION_CRASH"
const teamVerificationCrashManifest = "CORTEX_TEST_TEAM_VERIFICATION_MANIFEST"

type teamVerificationManifest struct {
	Database      string
	Scope         cortex.Scope
	Recipient     a2a.RecipientRequest
	Template      engine.VerifyRunRequest
	ProviderCalls int
}

type teamCaptureCrashStore struct {
	*expiringTeamCoordinator
	captured func(verification.Job)
}

func (s teamCaptureCrashStore) CaptureCandidate(ctx context.Context, request run.CandidateCapture) (verification.Job, error) {
	job, err := s.Store.CaptureCandidate(ctx, request)
	if err != nil {
		return verification.Job{}, err
	}
	s.captured(job)
	return job, nil
}

// TestPreparedTeamVerificationCrashWorker parks after private capture commits.
func TestPreparedTeamVerificationCrashWorker(t *testing.T) {
	if os.Getenv(teamVerificationCrashBoundary) == "" {
		return
	}
	path := os.Getenv(teamVerificationCrashManifest)
	require.NotEmpty(t, path)
	f := newVerifiedTeamFixture(t)
	wrapped := teamCaptureCrashStore{expiringTeamCoordinator: &expiringTeamCoordinator{Store: f.worker.storage}, captured: func(job verification.Job) {
		require.Equal(t, verification.JobQueued, job.State)
		require.Equal(t, 1, f.client.calls)
		require.Zero(t, f.worker.adapter.evaluations)
		data, err := json.Marshal(teamVerificationManifest{Database: f.worker.path, Scope: f.policy.Scope, Recipient: a2a.RecipientRequest{OrchestrationID: f.policy.ExecutionID, RunID: job.Request.Binding.RunID, AgentName: "a"}, Template: f.worker.request, ProviderCalls: f.client.calls})
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path+".tmp", data, 0600))
		require.NoError(t, os.Rename(path+".tmp", path))
		timer := time.NewTimer(time.Minute)
		defer timer.Stop()
		<-timer.C
		t.Fatal("parent did not kill captured-result worker")
	}}
	_, err := f.engine(t, wrapped, true).RunPreparedOrchestration(f.worker.ctx, f.prepared, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.NoError(t, err)
	t.Fatal("worker did not reach capture boundary")
}

func TestPreparedTeamVerificationRecoveryAfterProcessCrash(t *testing.T) {
	m := killCrashWorkerAtBoundary(t, "candidate_captured", crashWorkerConfig{Test: "TestPreparedTeamVerificationCrashWorker", BoundaryEnv: teamVerificationCrashBoundary, ManifestEnv: teamVerificationCrashManifest}, func(m teamVerificationManifest) { require.Equal(t, 1, m.ProviderCalls) })
	storage := openSQLiteStoreForTest(t, m.Database)
	ctx := cortex.WithScope(t.Context(), m.Scope)
	adapter := &workerAdapter{status: verification.StatusPassed}
	w := workerFixture{ctx: ctx, storage: storage, path: m.Database, request: m.Template, config: workerConfiguration(adapter), adapter: adapter}
	f := &verifiedTeamFixture{worker: w, client: &preparedTeamClient{}, config: requiredConfiguration(t, w), contentAllowed: true}
	fresh := f.engine(t, storage, true)
	allow := func(context.Context, id.OrchestrationID, id.AgentRunID) error { return nil }
	pending, err := fresh.ObservePreparedRecipient(ctx, m.Recipient, allow)
	require.NoError(t, err)
	require.Equal(t, a2a.RecipientWaiting, pending.State)
	require.Empty(t, pending.Output)
	page, err := fresh.RecoverVerificationPage(ctx, recoveryRequest())
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, engine.RecoveryCommitted, page.Items[0].Status)
	require.Zero(t, f.client.calls)
	recovered, err := fresh.ObservePreparedRecipient(ctx, m.Recipient, allow)
	require.NoError(t, err)
	require.Equal(t, a2a.RecipientSucceeded, recovered.State)
	require.Equal(t, "answer", recovered.Output)
	lease, err := storage.GetOrchestrationCoordinator(ctx, m.Recipient.OrchestrationID)
	require.NoError(t, err)
	// Only the fixture lease is shortened. Production recovery still waits for
	// its durable lease; killing a local process cannot bypass coordinator fencing.
	timer := time.NewTimer(time.Until(lease.ExpiresAt.Add(20 * time.Millisecond)))
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	completed, err := fresh.ResumePreparedOrchestration(ctx, m.Recipient.OrchestrationID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusCompleted, completed.Status)
	require.Len(t, completed.AgentRunIDs, 2)
	require.Equal(t, m.Recipient.RunID, completed.AgentRunIDs[0])
	require.Equal(t, 1, f.client.calls, "only the next child may generate")
	require.Equal(t, 2, adapter.evaluations)
}
