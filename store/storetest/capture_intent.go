package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func captureIntentStoreFixture(t *testing.T, s store.Store) (context.Context, verification.CaptureIntent, string) {
	t.Helper()
	ctx, template := captureJobFixture(t, s)
	current, err := s.GetRun(ctx, template.Binding.RunID)
	require.NoError(t, err)
	output := current.Output
	current.State = run.StateRunning
	current.Output = ""
	current.StopReason = ""
	current.CompletedAt = nil
	require.NoError(t, s.UpdateRun(ctx, current))
	template.ExpectedRunRevision = current.Revision
	template.Binding.Candidate.SHA256 = evidence.Digest(nil)
	template.Binding.Candidate.Bytes = 0
	template.EvaluationBudget = &verification.EvaluationBudget{MaxCalls: 2, MaxTokens: 100, TokenOnly: true}
	intent, err := verification.SealCaptureIntent(verification.CaptureIntent{SchemaVersion: 1, ID: id.New(verification.PrefixCaptureIntent), Template: template, ResultID: id.New(verification.PrefixResult), MaxCandidateBytes: 1 << 20})
	require.NoError(t, err)
	return ctx, intent, output
}

func finishCaptureIntent(ctx context.Context, t *testing.T, s store.Store, intent verification.CaptureIntent, output string) {
	t.Helper()
	current, err := s.GetRun(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	current.State = run.StateCompleted
	current.StopReason = run.StopFinalAnswer
	current.Output = output
	require.NoError(t, s.UpdateRun(ctx, current))
}

func testCaptureIntentRecovery(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, intent, output := captureIntentStoreFixture(t, s)
	storage := resultCapability[verification.CaptureIntentStore](t, s)
	require.NoError(t, storage.CreateCaptureIntent(ctx, intent))
	require.NoError(t, storage.CreateCaptureIntent(ctx, intent))
	loaded, err := storage.GetRunCaptureIntent(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, intent, loaded)
	_, err = storage.GetCaptureIntent(ctxWithScope("other"), intent.ID)
	require.ErrorIs(t, err, verification.ErrNotFound)
	_, err = storage.GetRunCaptureIntent(ctxWithScope("other"), intent.Template.Binding.RunID)
	require.ErrorIs(t, err, verification.ErrNotFound)
	_, err = storage.CaptureFromIntent(ctxWithScope("other"), intent.ID)
	require.ErrorIs(t, err, verification.ErrNotFound)
	_, err = storage.GetCaptureIntent(context.Background(), intent.ID)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = storage.CaptureFromIntent(ctx, intent.ID)
	require.Error(t, err)
	finishCaptureIntent(ctx, t, s, intent, output)
	job, err := storage.CaptureFromIntent(ctx, intent.ID)
	require.NoError(t, err)
	require.NoError(t, job.Validate())
	require.Equal(t, intent.Template.ID, job.Request.ID)
	require.Equal(t, evidence.Digest([]byte(output)), job.Request.Binding.Candidate.SHA256)
	artifact, err := resultCapability[evidence.Storage](t, s).LoadEvidence(ctx, job.Request.Binding.Candidate)
	require.NoError(t, err)
	require.Equal(t, output, string(artifact.Data))
	require.NoError(t, storage.CreateCaptureIntent(ctx, intent), "lost registration reply remains recoverable")
	retried, err := storage.CaptureFromIntent(ctx, intent.ID)
	require.NoError(t, err)
	require.Equal(t, job, retried)
}

func testCaptureIntentDenied(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"candidate exists", "candidate deleted", "scope", "cancelled", "tampered", "completed", "usage", "revision", "prepared", "contract", "budget deadline", "expired", "too large", "same run", "same job", "same result"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			ctx, intent, output := captureIntentStoreFixture(t, s)
			storage := resultCapability[verification.CaptureIntentStore](t, s)
			callCtx := ctx
			switch mode {
			case "candidate exists":
				require.NoError(t, resultCapability[evidence.Storage](t, s).SaveEvidence(ctx, evidence.Artifact{Ref: intent.Template.Binding.Candidate}))
			case "candidate deleted":
				require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), intent.Template.Binding.Candidate, "revoked before registration"))
			case "scope":
				callCtx = ctxWithScope("other")
			case "cancelled":
				var cancel context.CancelFunc
				callCtx, cancel = context.WithCancel(ctx)
				cancel()
			case "tampered":
				intent.MaxCandidateBytes--
			case "completed":
				finishCaptureIntent(ctx, t, s, intent, output)
			case "usage", "budget deadline":
				current, err := s.GetRun(ctx, intent.Template.Binding.RunID)
				require.NoError(t, err)
				if mode == "usage" {
					current.Usage.ToolCalls = 1
				} else {
					current.Budget.Deadline = intent.Template.Deadline.Add(-time.Second)
				}
				require.NoError(t, s.UpdateRun(ctx, current))
				intent.Template.ExpectedRunRevision = current.Revision
			case "revision":
				intent.Template.ExpectedRunRevision++
			case "prepared":
				intent.Template.Binding.Context.PreparedDigest = evidence.Digest([]byte("another preparation"))
			case "contract":
				intent.Template.Binding.ContractDigest = evidence.Digest([]byte("another contract"))
			case "expired":
				intent.Template.Deadline = time.Now().Add(-time.Second)
			case "too large":
				intent.MaxCandidateBytes = (1 << 20) + 1
			case "same run":
				require.NoError(t, storage.CreateCaptureIntent(ctx, intent))
				intent.ID = id.New(verification.PrefixCaptureIntent)
			case "same job", "same result":
				_, other, _ := captureIntentStoreFixture(t, s)
				if mode == "same job" {
					other.Template.ID = intent.Template.ID
				} else {
					other.ResultID = intent.ResultID
				}
				other, err := verification.SealCaptureIntent(other)
				require.NoError(t, err)
				require.NoError(t, storage.CreateCaptureIntent(ctx, other))
			}
			if mode != "tampered" {
				var err error
				intent, err = verification.SealCaptureIntent(intent)
				require.NoError(t, err)
			}
			require.Error(t, storage.CreateCaptureIntent(callCtx, intent))
			_, err := storage.GetCaptureIntent(ctx, intent.ID)
			require.ErrorIs(t, err, verification.ErrNotFound)
		})
	}
}

func testCaptureIntentCannotChangeCapturePolicy(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, intent, output := captureIntentStoreFixture(t, s)
	storage := resultCapability[verification.CaptureIntentStore](t, s)
	require.NoError(t, storage.CreateCaptureIntent(ctx, intent))
	finishCaptureIntent(ctx, t, s, intent, output)
	current, err := s.GetRun(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	original, err := intent.JobFor(current.Revision, current.Output)
	require.NoError(t, err)
	for _, mutate := range []func(*verification.JobRequest){
		func(r *verification.JobRequest) { r.ID = id.New(verification.PrefixJob) },
		func(r *verification.JobRequest) { r.MaxAttempts++ },
		func(r *verification.JobRequest) { r.EvaluationBudget.MaxTokens++ },
		func(r *verification.JobRequest) { r.Binding.Candidate.Producer = "other" },
		func(r *verification.JobRequest) { r.Binding.Context.AuthorityRevision = "other" },
	} {
		changed, _, freezeErr := original.Freeze()
		require.NoError(t, freezeErr)
		mutate(&changed)
		_, captureErr := resultCapability[verification.CaptureStore](t, s).CaptureVerificationJob(ctx, changed)
		require.Error(t, captureErr)
	}
	_, foreign := resultFixture(t, s)
	foreign.ResultID = intent.ResultID
	_, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, foreign)
	require.ErrorIs(t, err, verification.ErrConflict)
	_, otherRequest := captureJobFixture(t, s)
	otherRequest.ID = intent.Template.ID
	_, err = resultCapability[verification.CaptureStore](t, s).CaptureVerificationJob(ctx, otherRequest)
	require.ErrorIs(t, err, verification.ErrConflict)
	job, err := storage.CaptureFromIntent(ctx, intent.ID)
	require.NoError(t, err)
	js := resultCapability[verification.JobStore](t, s)
	job, err = js.ClaimVerificationJob(ctx, verification.JobClaim{JobID: job.Request.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker", Duration: time.Minute})
	require.NoError(t, err)
	contract, err := resultCapability[verification.Store](t, s).GetContract(ctx, job.Request.ContractID)
	require.NoError(t, err)
	outcomes := make([]verification.Outcome, 0, len(contract.Criteria))
	for _, criterion := range contract.Criteria {
		outcomes = append(outcomes, verification.Outcome{CriterionID: criterion.ID, Verifier: criterion.Verifier, VerifierRevision: criterion.VerifierRevision, Status: verification.StatusPassed})
	}
	now := time.Now().UTC()
	report, err := verification.SealReport(verification.Report{SchemaVersion: 1, ID: id.New(verification.PrefixReport), Binding: job.Request.Binding, CheckedAt: now, ExpiresAt: now.Add(time.Minute), Outcomes: outcomes})
	require.NoError(t, err)
	calls := resultCapability[verification.CallStore](t, s)
	call := callFor(report, 10, 0)
	job, err = calls.BeginVerificationCall(ctx, *job.Lease, call)
	require.NoError(t, err)
	job, err = calls.CompleteVerificationCall(ctx, *job.Lease, verification.CallResult{CallID: call.ID, Report: &report})
	require.NoError(t, err)
	request := jobCommitRequest(job)
	_, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
	require.ErrorIs(t, err, verification.ErrConflict)
	request.ResultID = intent.ResultID
	receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
	require.NoError(t, err)
	require.NoError(t, receipt.Validate())
}

// CaptureIntentRollback proves registration and later capture use real transactions.
func CaptureIntentRollback(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	storage := resultCapability[verification.CaptureIntentStore](t, s)
	for _, kind := range []string{"capture_intent", "capture_intent_run", "capture_intent_job", "capture_intent_result"} {
		t.Run(kind, func(t *testing.T) {
			ctx, intent, _ := captureIntentStoreFixture(t, s)
			failure := errors.New("registration interrupted")
			failed := assurance.New(captureFailDriver{Driver: driver, transaction: transaction, kind: kind, failure: failure})
			require.ErrorIs(t, failed.CreateCaptureIntent(ctx, intent), failure)
			_, err := storage.GetCaptureIntent(ctx, intent.ID)
			require.ErrorIs(t, err, verification.ErrNotFound)
			_, err = storage.GetRunCaptureIntent(ctx, intent.Template.Binding.RunID)
			require.ErrorIs(t, err, verification.ErrNotFound)
			require.NoError(t, storage.CreateCaptureIntent(ctx, intent))
		})
	}
	ctx, intent, output := captureIntentStoreFixture(t, s)
	require.NoError(t, storage.CreateCaptureIntent(ctx, intent))
	finishCaptureIntent(ctx, t, s, intent, output)
	failure := errors.New("capture interrupted")
	failed := assurance.New(failingRunDriver{Driver: driver, transaction: transaction, failure: failure})
	_, err := failed.CaptureFromIntent(ctx, intent.ID)
	require.ErrorIs(t, err, failure)
	_, err = resultCapability[verification.JobStore](t, s).GetVerificationJob(ctx, intent.Template.ID)
	require.ErrorIs(t, err, verification.ErrJobNotFound)
	current, err := s.GetRun(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	request, err := intent.JobFor(current.Revision, current.Output)
	require.NoError(t, err)
	_, err = resultCapability[evidence.Storage](t, s).LoadEvidence(ctx, request.Binding.Candidate)
	require.ErrorIs(t, err, evidence.ErrNotFound)
	retained, err := storage.GetRunCaptureIntent(ctx, current.ID)
	require.NoError(t, err)
	require.Equal(t, intent, retained)
	_, err = storage.CaptureFromIntent(ctx, intent.ID)
	require.NoError(t, err)
}

// CaptureIntentRestart recovers pre-generation policy from reopened storage.
func CaptureIntentRestart(t *testing.T, s store.Store, reopen func() store.Store) {
	t.Helper()
	ctx, intent, output := captureIntentStoreFixture(t, s)
	require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
	restored := reopen()
	storage := resultCapability[verification.CaptureIntentStore](t, restored)
	loaded, err := storage.GetRunCaptureIntent(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, intent, loaded)
	finishCaptureIntent(ctx, t, restored, loaded, output)
	job, err := storage.CaptureFromIntent(ctx, loaded.ID)
	require.NoError(t, err)
	require.Equal(t, intent.Template.ID, job.Request.ID)
	require.Equal(t, evidence.Digest([]byte(output)), job.Request.Binding.Candidate.SHA256)
}

func testCaptureIntentConcurrent(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"same intent", "same run", "same job", "same result"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			ctx, first, _ := captureIntentStoreFixture(t, s)
			second := first
			if mode == "same run" {
				second.ID = id.New(verification.PrefixCaptureIntent)
				second.Template.ID = id.New(verification.PrefixJob)
				second.ResultID = id.New(verification.PrefixResult)
			}
			if mode == "same job" || mode == "same result" {
				_, second, _ = captureIntentStoreFixture(t, s)
				if mode == "same job" {
					second.Template.ID = first.Template.ID
				} else {
					second.ResultID = first.ResultID
				}
			}
			var err error
			second, err = verification.SealCaptureIntent(second)
			require.NoError(t, err)
			storage := resultCapability[verification.CaptureIntentStore](t, s)
			type attempt struct {
				intent verification.CaptureIntent
				err    error
			}
			start := make(chan struct{})
			results := make(chan attempt, 2)
			for _, intent := range []verification.CaptureIntent{first, second} {
				go func() { <-start; results <- attempt{intent: intent, err: storage.CreateCaptureIntent(ctx, intent)} }()
			}
			close(start)
			successes := 0
			for range 2 {
				result := <-results
				if result.err == nil {
					successes++
				} else {
					require.ErrorIs(t, result.err, verification.ErrConflict)
					_, err = storage.GetCaptureIntent(ctx, result.intent.ID)
					require.ErrorIs(t, err, verification.ErrNotFound)
				}
			}
			expected := 1
			if mode == "same intent" {
				expected = 2
			}
			require.Equal(t, expected, successes)
		})
	}
}
