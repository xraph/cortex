package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func readLifecycle(ctx context.Context, t *testing.T, s store.Store, runID id.AgentRunID) event.Page {
	t.Helper()
	page, err := resultCapability[event.Store](t, s).ReadPublicEvents(ctx, event.ReadRequest{After: event.Cursor{SchemaVersion: 1, RunID: runID}, Limit: 20})
	require.NoError(t, err)
	return page
}

func requireLifecycle(t *testing.T, page event.Page, expected ...event.Type) {
	t.Helper()
	types := make([]event.Type, len(page.Events))
	seen := make(map[id.ID]bool)
	for i, item := range page.Events {
		types[i] = item.Type
		require.Equal(t, uint64(i+1), item.Sequence)
		require.False(t, seen[item.ID])
		seen[item.ID] = true
	}
	require.Equal(t, expected, types)
}

func testPrivateCaptureLifecycle(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, intent, capture := privateCandidateFixture(t, s)
	require.NoError(t, resultCapability[verification.CaptureIntentStore](t, s).CreateCaptureIntent(ctx, intent))
	cs := resultCapability[run.CandidateStore](t, s)
	job, err := cs.CaptureCandidate(ctx, capture)
	require.NoError(t, err)
	initial := readLifecycle(ctx, t, s, job.Request.Binding.RunID)
	requireLifecycle(t, initial, event.ExecutionFinished, event.VerificationPending)
	require.Equal(t, capture.ExpectedRunRevision+1, initial.Events[0].RunRevision)
	require.Equal(t, job.RunRevision, initial.Events[1].RunRevision)
	_, err = cs.CaptureCandidate(ctx, capture)
	require.NoError(t, err)
	require.Equal(t, initial, readLifecycle(ctx, t, s, job.Request.Binding.RunID))
	job = settlePrivateCandidate(ctx, t, s, job, verification.StatusPassed)
	require.Equal(t, initial, readLifecycle(ctx, t, s, job.Request.Binding.RunID), "ready is not a committed pass")
	request := jobCommitRequest(job)
	request.ResultID = intent.ResultID
	receipt, err := resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
	require.NoError(t, err)
	page := readLifecycle(ctx, t, s, job.Request.Binding.RunID)
	requireLifecycle(t, page, event.ExecutionFinished, event.VerificationPending, event.VerificationPassed, event.ResultCommitted)
	require.Equal(t, receipt.RunRevision, page.Events[2].RunRevision)
	require.Equal(t, receipt.RunRevision, page.Events[3].RunRevision)
	require.Equal(t, receipt.Request.ResultID, page.Events[2].ResultID)
	require.Equal(t, receipt.Outbox.EventID, page.Events[3].ID)
	_, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, request)
	require.NoError(t, err)
	require.Equal(t, page, readLifecycle(ctx, t, s, job.Request.Binding.RunID))
}

func testVerificationLifecycleOutcomes(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, status := range []verification.Status{verification.StatusFailed, verification.StatusInconclusive} {
		t.Run(string(status), func(t *testing.T) {
			s := newStore(t)
			ctx, req, reports := verificationJobFixture(t, s)
			js := resultCapability[verification.JobStore](t, s)
			job := claimCallJob(ctx, t, js, req)
			requireLifecycle(t, readLifecycle(ctx, t, s, req.Binding.RunID), event.VerificationPending)
			reports[0].Outcomes[0].Status = status
			report, err := verification.SealReport(reports[0])
			require.NoError(t, err)
			_, err = js.SaveVerificationJobReport(ctx, *job.Lease, report)
			require.NoError(t, err)
			page := readLifecycle(ctx, t, s, req.Binding.RunID)
			requireLifecycle(t, page, event.VerificationPending, event.Type("verification_"+string(status)))
			current, err := s.GetRun(ctx, req.Binding.RunID)
			require.NoError(t, err)
			require.Equal(t, current.Revision, page.Events[1].RunRevision)
			require.Empty(t, page.Events[1].ResultID)
			_, err = js.SaveVerificationJobReport(ctx, *job.Lease, report)
			require.Error(t, err)
			require.Equal(t, page, readLifecycle(ctx, t, s, req.Binding.RunID))
		})
	}
}

func testVerificationCancellationLifecycle(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, newer := range []bool{false, true} {
		s := newStore(t)
		ctx, req, _ := verificationJobFixture(t, s)
		js := resultCapability[verification.JobStore](t, s)
		_, err := js.EnqueueVerificationJob(ctx, req)
		require.NoError(t, err)
		if newer {
			current, readErr := s.GetRun(ctx, req.Binding.RunID)
			require.NoError(t, readErr)
			current.UpdatedAt = time.Now()
			require.NoError(t, s.UpdateRun(ctx, current))
		}
		_, err = js.CancelVerificationJob(ctx, req.ID)
		require.NoError(t, err)
		page := readLifecycle(ctx, t, s, req.Binding.RunID)
		if newer {
			requireLifecycle(t, page, event.VerificationPending)
		} else {
			requireLifecycle(t, page, event.VerificationPending, event.VerificationInconclusive)
		}
		_, err = js.CancelVerificationJob(ctx, req.ID)
		require.NoError(t, err)
		require.Equal(t, page, readLifecycle(ctx, t, s, req.Binding.RunID))
	}
}

func testVerificationProviderFailureLifecycle(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req, reports := verificationCallFixture(t, s, false)
	job := claimCallJob(ctx, t, resultCapability[verification.JobStore](t, s), req)
	calls := resultCapability[verification.CallStore](t, s)
	call := callFor(reports[0], 40, 40)
	job, err := calls.BeginVerificationCall(ctx, *job.Lease, call)
	require.NoError(t, err)
	_, err = calls.CompleteVerificationCall(ctx, *job.Lease, verification.CallResult{CallID: call.ID, Failure: "private provider diagnostics"})
	require.NoError(t, err)
	page := readLifecycle(ctx, t, s, req.Binding.RunID)
	requireLifecycle(t, page, event.VerificationPending, event.VerificationInconclusive)
	body, err := json.Marshal(page)
	require.NoError(t, err)
	require.NotContains(t, string(body), "private provider diagnostics")
	saved, err := resultCapability[verification.JobStore](t, s).GetVerificationJob(ctx, req.ID)
	require.NoError(t, err)
	require.Equal(t, verification.EvaluationDebit{Calls: 1, Tokens: 40, USDMicros: 40}, verification.DebitCalls(saved.Calls))
}

func verificationLifecycleRollback(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	failure := errors.New("after lifecycle writes")
	failed := assurance.New(failingRunDriver{Driver: driver, transaction: transaction, failure: failure})
	repository := assurance.New(driver)
	for _, mode := range []string{"report", "provider", "cancel"} {
		t.Run("lifecycle/"+mode, func(t *testing.T) {
			var ctx context.Context
			var req verification.JobRequest
			var reports []verification.Report
			if mode == "provider" {
				ctx, req, reports = verificationCallFixture(t, s, false)
			} else {
				ctx, req, reports = verificationJobFixture(t, s)
			}
			job := claimCallJob(ctx, t, repository, req)
			var call verification.CallRequest
			if mode == "provider" {
				call = callFor(reports[0], 40, 40)
				var err error
				job, err = repository.BeginVerificationCall(ctx, *job.Lease, call)
				require.NoError(t, err)
			}
			before := readLifecycle(ctx, t, s, req.Binding.RunID)
			finish := func(r *assurance.Repository) error {
				switch mode {
				case "provider":
					_, err := r.CompleteVerificationCall(ctx, *job.Lease, verification.CallResult{CallID: call.ID, Failure: "private failure"})
					return err
				case "cancel":
					_, err := r.CancelVerificationJob(ctx, req.ID)
					return err
				default:
					reports[0].Outcomes[0].Status = verification.StatusFailed
					report, err := verification.SealReport(reports[0])
					require.NoError(t, err)
					_, err = r.SaveVerificationJobReport(ctx, *job.Lease, report)
					return err
				}
			}
			require.ErrorIs(t, finish(failed), failure)
			require.Equal(t, before, readLifecycle(ctx, t, s, req.Binding.RunID))
			saved, err := repository.GetVerificationJob(ctx, req.ID)
			require.NoError(t, err)
			require.Equal(t, job, saved)
			require.NoError(t, finish(repository))
			expected := event.VerificationInconclusive
			if mode == "report" {
				expected = event.VerificationFailed
			}
			requireLifecycle(t, readLifecycle(ctx, t, s, req.Binding.RunID), event.VerificationPending, expected)
		})
	}
}
