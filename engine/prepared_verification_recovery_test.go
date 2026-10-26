package engine_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/sqlite"
	"github.com/xraph/cortex/verification"
)

type teamCaptureAckStore struct{ *sqlite.Store }

func (s teamCaptureAckStore) CaptureCandidate(ctx context.Context, req run.CandidateCapture) (verification.Job, error) {
	_, err := s.Store.CaptureCandidate(ctx, req)
	if err != nil {
		return verification.Job{}, err
	}
	return verification.Job{}, errors.New("capture acknowledgment lost")
}

type verifiedTeamFixture struct {
	worker         workerFixture
	client         *preparedTeamClient
	config         engine.RequiredVerificationConfig
	prepared       *engine.PreparedOrchestration
	policy         orchestration.BudgetPolicy
	contentAllowed bool
	redact         bool
	contentReads   int
}

func newVerifiedTeamFixture(t *testing.T) *verifiedTeamFixture {
	t.Helper()
	w := newWorkerFixture(t)
	f := &verifiedTeamFixture{worker: w, client: &preparedTeamClient{}, config: requiredConfiguration(t, w), contentAllowed: true}
	for _, name := range []string{"a", "b"} {
		require.NoError(t, w.storage.Create(w.ctx, &agent.Config{ID: id.NewAgentID(), Name: name, Model: "smart"}))
	}
	cfg := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "verified team", Strategy: orchestration.StrategySequential, Participants: []orchestration.Participant{{AgentName: "a"}, {AgentName: "b"}}, Settings: orchestration.Settings{Model: "smart"}}
	require.NoError(t, w.storage.CreateOrchestration(w.ctx, cfg))
	e := f.engine(t, w.storage, true)
	deadline := time.Now().Add(5 * time.Minute)
	budgets := map[string]execution.Budget{}
	allocations := map[string]execution.Resources{}
	for _, name := range []string{"a", "b"} {
		budgets[name] = execution.Budget{MaxSteps: 3, MaxTokens: 1000, Deadline: deadline}
		allocations[name] = execution.Resources{Steps: 3, Tokens: 1000, FanOut: 1}
	}
	var err error
	f.prepared, err = e.PrepareOrchestrationWithBudgets(w.ctx, cfg.ID, budgets)
	require.NoError(t, err)
	f.policy = orchestration.BudgetPolicy{SchemaVersion: 1, ExecutionID: id.NewOrchestrationID(), PreparedDigest: f.prepared.Digest(), Scope: cortex.ScopeFromContext(w.ctx), Limits: execution.Resources{Steps: 6, Tokens: 2000, FanOut: 2}, Participants: allocations, Deadline: deadline, TokenOnly: true}
	return f
}

func (f *verifiedTeamFixture) engine(t *testing.T, storage store.Store, content bool) *engine.Engine {
	t.Helper()
	opts := []engine.Option{engine.WithStore(storage), engine.WithLLM(f.client), engine.WithModelRevision("smart", "model:1"), engine.WithVerificationWorker(f.worker.config), engine.WithRequiredVerification(f.config), engine.WithPublicationGate(contentGate(func(_ context.Context, req safety.PublicationRequest) (safety.PublicationDecision, error) {
		if req.Source == "result_replay" && f.redact {
			return safety.PublicationDecision{Text: "changed answer", Status: safety.StatusRedacted}, nil
		}
		return safety.PublicationDecision{Text: req.Content, Status: safety.StatusApproved}, nil
	}))}
	if content {
		opts = append(opts, engine.WithResultContent(engine.ResultContentConfig{Authorizer: contentAuthorizer(func(context.Context, verification.ResultContentRequest) error {
			f.contentReads++
			if !f.contentAllowed {
				return errors.New("content revoked")
			}
			return nil
		}), Fence: f.worker.config.Fence, Clock: workerClock{}}))
	}
	e, err := engine.New(opts...)
	require.NoError(t, err)
	return e
}

func (f *verifiedTeamFixture) captured(t *testing.T) (*orchestration.Run, a2a.RecipientRequest) {
	t.Helper()
	e := f.engine(t, teamCaptureAckStore{f.worker.storage}, true)
	team, err := e.RunPreparedOrchestration(f.worker.ctx, f.prepared, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.NotNil(t, team)
	require.Len(t, team.AgentRunIDs, 1)
	child, readErr := f.worker.storage.GetRun(f.worker.ctx, team.AgentRunIDs[0])
	require.NoError(t, readErr)
	require.Equal(t, run.StateCompleted, child.State)
	require.Equal(t, verification.StatusPending, child.VerificationStatus)
	require.Empty(t, child.Output)
	require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
	require.Equal(t, orchestration.StatusWaiting, team.Status)
	require.Len(t, team.AgentRunIDs, 1)
	require.Equal(t, 1, f.client.calls)
	require.Zero(t, f.worker.adapter.evaluations)
	return team, a2a.RecipientRequest{OrchestrationID: team.ID, RunID: team.AgentRunIDs[0], AgentName: "a"}
}

func TestPreparedTeamRejectsFailedRecoveredVerification(t *testing.T) {
	for _, status := range []verification.Status{verification.StatusFailed, verification.StatusInconclusive} {
		t.Run(string(status), func(t *testing.T) {
			f := newVerifiedTeamFixture(t)
			team, recipient := f.captured(t)
			f.worker.adapter.status = status
			fresh := f.engine(t, f.worker.storage, true)
			page, err := fresh.RecoverVerificationPage(f.worker.ctx, recoveryRequest())
			require.NoError(t, err)
			require.Equal(t, engine.RecoveryTerminal, page.Items[0].Status)
			observed, err := fresh.ObservePreparedRecipient(f.worker.ctx, recipient, func(context.Context, id.OrchestrationID, id.AgentRunID) error { return nil })
			require.NoError(t, err)
			require.Equal(t, a2a.RecipientFailed, observed.State)
			require.Empty(t, observed.Output)
			failed, err := fresh.ResumePreparedOrchestration(f.worker.ctx, team.ID)
			require.ErrorIs(t, err, cortex.ErrRunUnverified)
			require.Equal(t, orchestration.StatusFailed, failed.Status)
			require.Equal(t, 1, f.client.calls)
			require.Zero(t, f.contentReads)
		})
	}
}

func TestPreparedTeamRecoveryWaitsWithoutReclaimingPendingVerification(t *testing.T) {
	f := newVerifiedTeamFixture(t)
	team, _ := f.captured(t)
	fresh := f.engine(t, f.worker.storage, true)
	source, err := coordination.NewScopedOrchestrationSource(f.worker.storage, []cortex.Scope{f.policy.Scope}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	var scans atomic.Int32
	require.NoError(t, engine.WithOrchestrationRecovery(engine.OrchestrationRecoveryConfig{Service: coordination.OrchestrationRecoveryConfig{Discovery: source, Interval: 10 * time.Millisecond}, Authorize: func(context.Context, id.OrchestrationID) error { scans.Add(1); return nil }})(fresh))
	require.NoError(t, fresh.Start(f.worker.ctx))
	t.Cleanup(func() { require.NoError(t, fresh.Stop(context.Background())) })
	require.Eventually(t, func() bool { return scans.Load() >= 8 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, fresh.Stop(context.Background()))
	after, err := f.worker.storage.GetOrchestrationRun(f.worker.ctx, team.ID)
	require.NoError(t, err)
	require.Equal(t, team.ContinuationGeneration, after.ContinuationGeneration)
	require.Equal(t, orchestration.StatusWaiting, after.Status)
	require.Equal(t, 1, f.client.calls)
	require.Zero(t, f.worker.adapter.evaluations)
}

func TestPreparedTeamRecoversCapturedVerificationBeforeAdvancing(t *testing.T) {
	f := newVerifiedTeamFixture(t)
	team, recipient := f.captured(t)
	ctx := f.worker.ctx
	reopened := openSQLiteStoreForTest(t, f.worker.path)
	fresh := f.engine(t, reopened, true)
	allow := func(context.Context, id.OrchestrationID, id.AgentRunID) error { return nil }
	pending, err := fresh.ObservePreparedRecipient(ctx, recipient, allow)
	require.NoError(t, err)
	require.Equal(t, a2a.RecipientWaiting, pending.State)
	require.Empty(t, pending.Output)
	require.Zero(t, f.contentReads)
	waiting, err := fresh.ResumePreparedOrchestration(ctx, team.ID)
	require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
	require.Equal(t, orchestration.StatusWaiting, waiting.Status)
	require.Equal(t, 1, f.client.calls)
	page, err := fresh.RecoverVerificationPage(ctx, recoveryRequest())
	require.NoError(t, err)
	require.Len(t, page.Items, 1)
	require.Equal(t, engine.RecoveryCommitted, page.Items[0].Status)
	require.Equal(t, 1, f.client.calls)
	observed, err := fresh.ObservePreparedRecipient(ctx, recipient, allow)
	require.NoError(t, err)
	require.Equal(t, a2a.RecipientSucceeded, observed.State)
	require.Equal(t, "answer", observed.Output)
	require.Positive(t, f.contentReads)
	completed, err := fresh.ResumePreparedOrchestration(ctx, team.ID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusCompleted, completed.Status)
	require.Equal(t, 2, f.client.calls, "recovery must not regenerate the first child")
	require.Equal(t, 2, f.worker.adapter.evaluations)
}

func TestPreparedTeamRecoveredResultsRequireCurrentContentAccess(t *testing.T) {
	for _, mode := range []string{"denied", "reader_missing", "redacted", "downgraded", "missing_result", "wrong_result"} {
		t.Run(mode, func(t *testing.T) {
			f := newVerifiedTeamFixture(t)
			_, recipient := f.captured(t)
			fresh := f.engine(t, f.worker.storage, mode != "reader_missing")
			page, err := fresh.RecoverVerificationPage(f.worker.ctx, recoveryRequest())
			require.NoError(t, err)
			require.Equal(t, engine.RecoveryCommitted, page.Items[0].Status)
			switch mode {
			case "denied":
				f.contentAllowed = false
			case "redacted":
				f.redact = true
			case "downgraded", "missing_result", "wrong_result":
				child, readErr := f.worker.storage.GetRun(f.worker.ctx, recipient.RunID)
				require.NoError(t, readErr)
				if mode == "downgraded" {
					child.Provenance.VerificationRequired = false
				}
				if mode == "missing_result" {
					child.ResultRef = ""
				}
				if mode == "wrong_result" {
					child.ResultRef = id.New(verification.PrefixResult).String()
				}
				require.NoError(t, f.worker.storage.UpdateRun(f.worker.ctx, child))
			}
			got, err := fresh.ObservePreparedRecipient(f.worker.ctx, recipient, func(context.Context, id.OrchestrationID, id.AgentRunID) error { return nil })
			require.Error(t, err)
			require.Zero(t, got)
			require.Equal(t, 1, f.client.calls)
		})
	}
}
