package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func testPrivateCandidate(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, intent, output := captureIntentStoreFixture(t, s)
	intents := resultCapability[verification.CaptureIntentStore](t, s)
	require.NoError(t, intents.CreateCaptureIntent(ctx, intent))
	capture := run.CandidateCapture{SchemaVersion: 1, IntentID: intent.ID, ExpectedRunRevision: intent.Template.ExpectedRunRevision, Output: output, FinishReason: "stop", Usage: execution.Usage{Steps: 1, Tokens: 5, ReportedTokens: 5}}
	cs := resultCapability[run.CandidateStore](t, s)
	job, err := cs.CaptureCandidate(ctx, capture)
	require.NoError(t, err)
	require.Equal(t, verification.JobQueued, job.State)
	require.Equal(t, capture.ExpectedRunRevision+2, job.RunRevision)
	saved, err := s.GetRun(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	require.Empty(t, saved.Output)
	require.Empty(t, saved.ResultRef)
	require.Equal(t, verification.StatusPending, saved.VerificationStatus)
	require.Equal(t, run.StateCompleted, saved.State)
	require.Equal(t, capture.Usage, saved.Usage)
	require.Equal(t, capture.Usage.Steps, saved.StepCount)
	require.Equal(t, capture.Usage.Tokens, saved.TokensUsed)
	require.NotNil(t, saved.CompletedAt)
	listed, err := s.ListRuns(ctx, &run.ListFilter{})
	require.NoError(t, err)
	for _, r := range listed {
		require.Empty(t, r.Output)
	}
	artifact, err := resultCapability[evidence.Storage](t, s).LoadEvidence(ctx, job.Request.Binding.Candidate)
	require.NoError(t, err)
	require.Equal(t, output, string(artifact.Data))
	retried, err := cs.CaptureCandidate(ctx, capture)
	require.NoError(t, err)
	require.Equal(t, job, retried)
	retried, err = intents.CaptureFromIntent(ctx, intent.ID)
	require.NoError(t, err)
	require.Equal(t, job, retried)
	job = settlePrivateCandidate(ctx, t, s, job, verification.StatusPassed)
	saved, err = s.GetRun(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	require.Empty(t, saved.Output, "passing checks alone do not publish")
	request := jobCommitRequest(job)
	request.ResultID = intent.ResultID
	receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
	require.NoError(t, err)
	require.NoError(t, receipt.Validate())
	saved, err = s.GetRun(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, output, saved.Output)
	require.Equal(t, verification.StatusPassed, saved.VerificationStatus)
	require.Equal(t, intent.ResultID.String(), saved.ResultRef)
	retried, err = cs.CaptureCandidate(ctx, capture)
	require.NoError(t, err)
	require.Equal(t, verification.JobCommitted, retried.State)
}

func settlePrivateCandidate(ctx context.Context, t *testing.T, s store.Store, job verification.Job, status verification.Status) verification.Job {
	t.Helper()
	job = claimCallJob(ctx, t, resultCapability[verification.JobStore](t, s), job.Request)
	contract, err := resultCapability[verification.Store](t, s).GetContract(ctx, job.Request.ContractID)
	require.NoError(t, err)
	outcomes := make([]verification.Outcome, 0, len(contract.Criteria))
	for _, criterion := range contract.Criteria {
		outcomes = append(outcomes, verification.Outcome{CriterionID: criterion.ID, Verifier: criterion.Verifier, VerifierRevision: criterion.VerifierRevision, Status: status})
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
	return job
}

func privateCandidateFixture(t *testing.T, s store.Store) (context.Context, verification.CaptureIntent, run.CandidateCapture) {
	t.Helper()
	ctx, intent, output := captureIntentStoreFixture(t, s)
	capture := run.CandidateCapture{SchemaVersion: 1, IntentID: intent.ID, ExpectedRunRevision: intent.Template.ExpectedRunRevision, Output: output, FinishReason: "stop", Usage: execution.Usage{Steps: 1, Tokens: 5, ReportedTokens: 5}}
	return ctx, intent, capture
}

func testPrivateCandidateDenied(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"scope", "no scope", "cancelled", "missing intent", "revision", "state", "output", "prepared", "contract", "deadline", "steps", "tokens", "tools", "usage", "estimated", "too large", "intent bound", "deleted", "conflicting evidence"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			ctx, intent, capture := privateCandidateFixture(t, s)
			if mode == "intent bound" {
				intent.MaxCandidateBytes = 1
				var err error
				intent, err = verification.SealCaptureIntent(intent)
				require.NoError(t, err)
			}
			if mode != "missing intent" {
				require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
			}
			current, err := s.GetRun(ctx, intent.Template.Binding.RunID)
			require.NoError(t, err)
			callCtx := ctx
			switch mode {
			case "scope":
				callCtx = ctxWithScope("other")
			case "no scope":
				callCtx = context.Background()
			case "cancelled":
				var cancel context.CancelFunc
				callCtx, cancel = context.WithCancel(ctx)
				cancel()
			case "revision":
				capture.ExpectedRunRevision++
			case "too large":
				capture.Output = strings.Repeat("x", (1<<20)+1)
			case "deleted":
				require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), intent.Template.Binding.Candidate, "revoked"))
			case "conflicting evidence":
				artifact, sealErr := evidence.Seal(evidence.Artifact{Ref: intent.Template.Binding.Candidate, Data: []byte("other")})
				require.NoError(t, sealErr)
				require.NoError(t, resultCapability[evidence.Storage](t, s).SaveEvidence(ctx, artifact))
			case "state", "output", "prepared", "contract", "deadline", "steps", "tokens", "tools", "usage", "estimated":
				switch mode {
				case "state":
					current.State = run.StateCancelled
				case "output":
					current.Output = "already public"
				case "prepared":
					current.Provenance.PreparedDigest = evidence.Digest([]byte("changed"))
				case "contract":
					current.Provenance.AcceptanceRef = id.New(verification.PrefixContract).String()
				case "deadline":
					current.Budget.Deadline = intent.Template.Deadline.Add(-time.Second)
				case "steps":
					current.Budget.MaxSteps = 1
					capture.Usage.Steps = 2
				case "tokens":
					current.Budget.MaxTokens = 4
				case "tools":
					current.Budget.MaxToolCalls = 1
					capture.Usage.ToolCalls = 2
				case "usage":
					current.Usage = execution.Usage{Steps: 2, Tokens: 6, ReportedTokens: 6, ToolCalls: 1}
				case "estimated":
					current.Usage.Estimated = true
				}
				require.NoError(t, s.UpdateRun(ctx, current))
				capture.ExpectedRunRevision = current.Revision
			}
			before, err := s.GetRun(ctx, current.ID)
			require.NoError(t, err)
			_, err = resultCapability[run.CandidateStore](t, s).CaptureCandidate(callCtx, capture)
			require.Error(t, err)
			after, err := s.GetRun(ctx, current.ID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			_, err = resultCapability[verification.JobStore](t, s).GetVerificationJob(ctx, intent.Template.ID)
			require.ErrorIs(t, err, verification.ErrJobNotFound)
			if mode != "deleted" && mode != "conflicting evidence" {
				req, reqErr := intent.JobFor(capture.ExpectedRunRevision+1, "private candidate text")
				if reqErr == nil {
					_, err = resultCapability[evidence.Storage](t, s).LoadEvidence(ctx, req.Binding.Candidate)
					require.ErrorIs(t, err, evidence.ErrNotFound)
				}
			}
		})
	}
}

func testPrivateCandidateCannotPublish(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"failed", "inconclusive", "cancelled", "deleted", "changed output", "changed candidate", "wrong result"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			ctx, intent, capture := privateCandidateFixture(t, s)
			require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
			job, err := resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, capture)
			require.NoError(t, err)
			status := verification.StatusPassed
			if mode == "failed" {
				status = verification.StatusFailed
			}
			if mode == "inconclusive" {
				status = verification.StatusInconclusive
			}
			job = settlePrivateCandidate(ctx, t, s, job, status)
			request := jobCommitRequest(job)
			request.ResultID = intent.ResultID
			switch mode {
			case "cancelled":
				_, err = resultCapability[verification.JobStore](t, s).CancelVerificationJob(ctx, job.Request.ID)
				require.NoError(t, err)
			case "deleted":
				require.NoError(t, resultCapability[evidence.Storage](t, s).TombstoneEvidence(evidence.WithDeletionActor(ctx, "operator"), job.Request.Binding.Candidate, "revoked"))
			case "changed output":
				current, readErr := s.GetRun(ctx, intent.Template.Binding.RunID)
				require.NoError(t, readErr)
				current.Output = "different"
				require.NoError(t, s.UpdateRun(ctx, current))
			case "changed candidate":
				request.Current.Candidate.SHA256 = evidence.Digest([]byte("another answer"))
			case "wrong result":
				request.ResultID = id.New(verification.PrefixResult)
			}
			before, err := s.GetRun(ctx, intent.Template.Binding.RunID)
			require.NoError(t, err)
			_, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
			require.Error(t, err)
			after, err := s.GetRun(ctx, intent.Template.Binding.RunID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Empty(t, after.ResultRef)
			require.NotEqual(t, verification.StatusPassed, after.VerificationStatus)
			_, err = resultCapability[verification.CommitStore](t, s).GetCommittedResult(ctx, intent.ResultID)
			require.ErrorIs(t, err, verification.ErrNotFound)
			retry, err := resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, capture)
			require.NoError(t, err)
			require.Equal(t, job.RequestDigest, retry.RequestDigest)
			changed := capture
			changed.Output += " changed"
			_, err = resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, changed)
			require.ErrorIs(t, err, verification.ErrConflict)
		})
	}
}

// PrivateCandidateRollback verifies capture and publication using real transactions.
func PrivateCandidateRollback(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Run("transcript", func(t *testing.T) { transcriptCaptureRollback(t, s, driver) })
	t.Helper()
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	failure := errors.New("interrupted private candidate transaction")
	t.Run("lost reply", func(t *testing.T) {
		ctx, intent, capture := privateCandidateFixture(t, s)
		require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
		failed := assurance.New(privateLostReplyDriver{Driver: driver, transaction: transaction, failure: failure})
		job, err := failed.CaptureCandidate(ctx, capture)
		require.ErrorIs(t, err, failure)
		require.Empty(t, job)
		job, err = resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, capture)
		require.NoError(t, err)
		require.Equal(t, verification.JobQueued, job.State)
		saved, err := s.GetRun(ctx, intent.Template.Binding.RunID)
		require.NoError(t, err)
		require.Empty(t, saved.Output)
		job = settlePrivateCandidate(ctx, t, s, job, verification.StatusPassed)
		request := jobCommitRequest(job)
		request.ResultID = intent.ResultID
		receipt, err := failed.CommitVerifiedResult(ctx, request)
		require.ErrorIs(t, err, failure)
		require.Empty(t, receipt)
		receipt, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
		require.NoError(t, err)
		require.Equal(t, intent.ResultID, receipt.Request.ResultID)
		saved, err = s.GetRun(ctx, intent.Template.Binding.RunID)
		require.NoError(t, err)
		require.Equal(t, capture.Output, saved.Output)
	})
	for _, kind := range []string{assurance.KindEvidence, "private_candidate", "candidate_run", assurance.KindJob, "verification_job_binding", "run", assurance.KindEventStream, "public_event_identity"} {
		t.Run("capture/"+kind, func(t *testing.T) {
			ctx, intent, capture := privateCandidateFixture(t, s)
			require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
			before, err := s.GetRun(ctx, intent.Template.Binding.RunID)
			require.NoError(t, err)
			failed := assurance.New(captureFailDriver{Driver: driver, transaction: transaction, kind: kind, failure: failure})
			job, err := failed.CaptureCandidate(ctx, capture)
			require.ErrorIs(t, err, failure)
			require.Empty(t, job)
			after, err := s.GetRun(ctx, intent.Template.Binding.RunID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			require.Empty(t, readLifecycle(ctx, t, s, intent.Template.Binding.RunID).Events)
			_, err = resultCapability[verification.JobStore](t, s).GetVerificationJob(ctx, intent.Template.ID)
			require.ErrorIs(t, err, verification.ErrJobNotFound)
			req, err := intent.JobFor(capture.ExpectedRunRevision+1, capture.Output)
			require.NoError(t, err)
			_, err = resultCapability[evidence.Storage](t, s).LoadEvidence(ctx, req.Binding.Candidate)
			require.ErrorIs(t, err, evidence.ErrNotFound)
			_, err = resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, capture)
			require.NoError(t, err)
		})
	}
	for _, kind := range []string{"result", "candidate_run", "commit_job", assurance.KindEventStream, "public_event_identity"} {
		t.Run("commit/"+kind, func(t *testing.T) {
			ctx, intent, capture := privateCandidateFixture(t, s)
			require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
			job, err := resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, capture)
			require.NoError(t, err)
			job = settlePrivateCandidate(ctx, t, s, job, verification.StatusPassed)
			before, err := s.GetRun(ctx, intent.Template.Binding.RunID)
			require.NoError(t, err)
			request := jobCommitRequest(job)
			request.ResultID = intent.ResultID
			failed := assurance.New(captureFailDriver{Driver: driver, transaction: transaction, kind: kind, failure: failure})
			receipt, err := failed.CommitVerifiedResult(ctx, request)
			require.ErrorIs(t, err, failure)
			require.Empty(t, receipt)
			after, err := s.GetRun(ctx, intent.Template.Binding.RunID)
			require.NoError(t, err)
			require.Equal(t, before, after)
			requireLifecycle(t, readLifecycle(ctx, t, s, intent.Template.Binding.RunID), event.ExecutionFinished, event.VerificationPending)
			require.Empty(t, after.Output)
			_, err = resultCapability[verification.CommitStore](t, s).GetCommittedResult(ctx, intent.ResultID)
			require.ErrorIs(t, err, verification.ErrNotFound)
			saved, err := resultCapability[verification.JobStore](t, s).GetVerificationJob(ctx, job.Request.ID)
			require.NoError(t, err)
			require.Equal(t, job, saved)
			_, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
			require.NoError(t, err)
		})
	}
}

// PrivateCandidateRestart recovers capture and publishes only after fresh commit.
func PrivateCandidateRestart(t *testing.T, s store.Store, reopen func() store.Store) {
	t.Helper()
	ctx, intent, capture := privateCandidateFixture(t, s)
	require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
	original, err := resultCapability[run.CandidateStore](t, s).CaptureCandidate(ctx, capture)
	require.NoError(t, err)
	restored := reopen()
	job, err := resultCapability[run.CandidateStore](t, restored).CaptureCandidate(ctx, capture)
	require.NoError(t, err)
	require.Equal(t, original, job)
	saved, err := restored.GetRun(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	require.Empty(t, saved.Output)
	job = settlePrivateCandidate(ctx, t, restored, job, verification.StatusPassed)
	request := jobCommitRequest(job)
	request.ResultID = intent.ResultID
	_, err = resultCapability[verification.CommitStore](t, restored).CommitVerifiedResult(ctx, request)
	require.NoError(t, err)
	saved, err = restored.GetRun(ctx, intent.Template.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, capture.Output, saved.Output)
}

func testPrivateCandidateConcurrent(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, same := range []bool{true, false} {
		t.Run(map[bool]string{true: "same", false: "different"}[same], func(t *testing.T) {
			s := newStore(t)
			ctx, intent, first := privateCandidateFixture(t, s)
			require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
			second := first
			if !same {
				second.Output += " changed"
			}
			type result struct {
				job verification.Job
				err error
			}
			results := make(chan result, 2)
			start := make(chan struct{})
			captures := resultCapability[run.CandidateStore](t, s)
			for _, request := range []run.CandidateCapture{first, second} {
				go func() { <-start; job, err := captures.CaptureCandidate(ctx, request); results <- result{job, err} }()
			}
			close(start)
			successes := 0
			for range 2 {
				result := <-results
				if result.err == nil {
					successes++
					require.Equal(t, verification.JobQueued, result.job.State)
				} else {
					require.ErrorIs(t, result.err, verification.ErrConflict)
				}
			}
			expected := 1
			if same {
				expected = 2
			}
			require.Equal(t, expected, successes)
			saved, err := s.GetRun(ctx, intent.Template.Binding.RunID)
			require.NoError(t, err)
			require.Empty(t, saved.Output)
			require.Equal(t, first.ExpectedRunRevision+2, saved.Revision)
		})
	}
}

// Lost-reply injection returns an error only after the database committed.
type privateLostReplyDriver struct {
	assurance.Driver
	transaction assurance.RunDriver
	failure     error
}

func (d privateLostReplyDriver) TransactRun(ctx context.Context, runID id.AgentRunID, callback func(context.Context, assurance.RunTransaction) error) error {
	if err := d.transaction.TransactRun(ctx, runID, callback); err != nil {
		return err
	}
	return d.failure
}
