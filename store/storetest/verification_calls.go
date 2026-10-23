package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

func verificationCallFixture(t *testing.T, s store.Store, optional bool) (context.Context, verification.JobRequest, []verification.Report) {
	t.Helper()
	ctx, req, reports := verificationJobFixture(t, s)
	vs := resultCapability[verification.Store](t, s)
	contract, err := vs.GetContract(ctx, req.ContractID)
	require.NoError(t, err)
	contract.ID = id.New(verification.PrefixContract)
	contract.Criteria[1].Verifier = "second-checker"
	contract.Criteria[1].Required = !optional
	contract, err = verification.SealContract(contract)
	require.NoError(t, err)
	require.NoError(t, vs.CreateContract(ctx, contract))
	current, err := s.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	current.Provenance.AcceptanceRef = contract.ID.String()
	require.NoError(t, s.UpdateRun(ctx, current))
	req.ExpectedRunRevision = current.Revision
	req.ContractID = contract.ID
	req.Binding.ContractDigest = contract.Digest
	req.EvaluationBudget = &verification.EvaluationBudget{MaxCalls: 2, MaxTokens: 100, MaxUSDMicros: 100, PricingRevision: "host-prices-1"}
	for i := range reports {
		reports[i].Binding = req.Binding
		reports[i].Outcomes[0].Verifier = contract.Criteria[i].Verifier
		reports[i], err = verification.SealReport(reports[i])
		require.NoError(t, err)
	}
	return ctx, req, reports
}

func callFor(report verification.Report, tokens, money int64) verification.CallRequest {
	return verification.CallRequest{ID: id.New(verification.PrefixCall), Verifier: report.Outcomes[0].Verifier, VerifierRevision: report.Outcomes[0].VerifierRevision, MaxTokens: tokens, MaxUSDMicros: money}
}

func claimCallJob(ctx context.Context, t *testing.T, js verification.JobStore, req verification.JobRequest) verification.Job {
	t.Helper()
	_, err := js.EnqueueVerificationJob(ctx, req)
	require.NoError(t, err)
	job, err := js.ClaimVerificationJob(ctx, verification.JobClaim{JobID: req.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker", Duration: time.Minute})
	require.NoError(t, err)
	return job
}

func testVerificationCalls(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req, reports := verificationCallFixture(t, s, false)
	js := resultCapability[verification.JobStore](t, s)
	cs := resultCapability[verification.CallStore](t, s)
	job := claimCallJob(ctx, t, js, req)
	lease := *job.Lease
	first, second := callFor(reports[0], 80, 70), callFor(reports[1], 30, 40)
	_, err := cs.BeginVerificationCall(context.Background(), lease, first)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	_, err = cs.BeginVerificationCall(ctxWithScope("other"), lease, first)
	require.ErrorIs(t, err, verification.ErrJobNotFound)
	job, err = cs.BeginVerificationCall(ctx, lease, first)
	require.NoError(t, err)
	require.Len(t, job.Calls, 1)
	require.NoError(t, job.Validate())
	require.Equal(t, verification.EvaluationDebit{Calls: 1, Tokens: 80, USDMicros: 70}, verification.DebitCalls(job.Calls))
	_, err = js.SaveVerificationJobReport(ctx, lease, reports[0])
	require.ErrorIs(t, err, verification.ErrJobConflict)
	_, err = cs.BeginVerificationCall(ctx, lease, first)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	_, err = cs.BeginVerificationCall(ctx, lease, second)
	require.ErrorIs(t, err, verification.ErrJobBudgetExceeded)
	result := verification.CallResult{CallID: first.ID, Report: &reports[0], Usage: verification.EvaluationUsage{Tokens: 20, TokensKnown: true, Receipt: "meter-1"}}
	_, err = cs.CompleteVerificationCall(ctxWithScope("other"), lease, result)
	require.ErrorIs(t, err, verification.ErrJobNotFound)
	job, err = cs.CompleteVerificationCall(ctx, lease, result)
	require.NoError(t, err)
	require.Equal(t, verification.JobRunning, job.State)
	retry, err := cs.CompleteVerificationCall(ctx, lease, result)
	require.NoError(t, err)
	require.Equal(t, job, retry)
	require.Equal(t, verification.EvaluationDebit{Calls: 1, Tokens: 20, USDMicros: 70}, verification.DebitCalls(job.Calls))
	second.MaxTokens, second.MaxUSDMicros = 80, 30
	job, err = cs.BeginVerificationCall(ctx, lease, second)
	require.NoError(t, err)
	job, err = cs.CompleteVerificationCall(ctx, lease, verification.CallResult{CallID: second.ID, Report: &reports[1], Usage: verification.EvaluationUsage{USDMicros: 10, MoneyKnown: true, Receipt: "meter-2"}})
	require.NoError(t, err)
	require.Equal(t, verification.JobReady, job.State)
	require.Equal(t, verification.EvaluationDebit{Calls: 2, Tokens: 100, USDMicros: 80}, verification.DebitCalls(job.Calls))
	_, err = resultCapability[verification.CommitStore](t, s).CommitVerifiedResult(ctx, jobCommitRequest(job))
	require.NoError(t, err)
}

func testVerificationCallsOptionalPending(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx, req, reports := verificationCallFixture(t, s, true)
	js := resultCapability[verification.JobStore](t, s)
	cs := resultCapability[verification.CallStore](t, s)
	job := claimCallJob(ctx, t, js, req)
	lease := *job.Lease
	first, second := callFor(reports[0], 10, 10), callFor(reports[1], 10, 10)
	_, err := cs.BeginVerificationCall(ctx, lease, first)
	require.NoError(t, err)
	_, err = cs.BeginVerificationCall(ctx, lease, second)
	require.NoError(t, err)
	job, err = cs.CompleteVerificationCall(ctx, lease, verification.CallResult{CallID: first.ID, Report: &reports[0]})
	require.NoError(t, err)
	require.Equal(t, verification.JobRunning, job.State)
	reports[1].Outcomes[0].Status = verification.StatusInconclusive
	reports[1], err = verification.SealReport(reports[1])
	require.NoError(t, err)
	job, err = cs.CompleteVerificationCall(ctx, lease, verification.CallResult{CallID: second.ID, Report: &reports[1]})
	require.NoError(t, err)
	require.Equal(t, verification.JobReady, job.State)
}

func testVerificationCallsDenials(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"overrun", "failed evaluator", "wrong report", "changed receipt", "unknown group", "concurrent dispatch", "concurrent budget"} {
		t.Run(mode, func(t *testing.T) {
			s := newStore(t)
			ctx, req, reports := verificationCallFixture(t, s, false)
			js := resultCapability[verification.JobStore](t, s)
			cs := resultCapability[verification.CallStore](t, s)
			job := claimCallJob(ctx, t, js, req)
			lease := *job.Lease
			call := callFor(reports[0], 40, 40)
			if mode == "concurrent budget" {
				results := make(chan error, 2)
				start := make(chan struct{})
				for _, report := range reports {
					go func() {
						<-start
						_, err := cs.BeginVerificationCall(ctx, lease, callFor(report, 60, 60))
						results <- err
					}()
				}
				close(start)
				winners := 0
				for range 2 {
					if err := <-results; err == nil {
						winners++
					} else {
						require.ErrorIs(t, err, verification.ErrJobBudgetExceeded)
					}
				}
				require.Equal(t, 1, winners)
				return
			}
			if mode == "unknown group" {
				call.Verifier = "unregistered"
				_, err := cs.BeginVerificationCall(ctx, lease, call)
				require.ErrorIs(t, err, verification.ErrInvalid)
				saved, err := js.GetVerificationJob(ctx, req.ID)
				require.NoError(t, err)
				require.Equal(t, job, saved)
				return
			}
			if mode == "concurrent dispatch" {
				results := make(chan error, 4)
				start := make(chan struct{})
				for range 4 {
					go func() { <-start; _, err := cs.BeginVerificationCall(ctx, lease, call); results <- err }()
				}
				close(start)
				winners := 0
				for range 4 {
					if err := <-results; err == nil {
						winners++
					} else {
						require.ErrorIs(t, err, verification.ErrCallUncertain)
					}
				}
				require.Equal(t, 1, winners)
				return
			}
			job, err := cs.BeginVerificationCall(ctx, lease, call)
			require.NoError(t, err)
			result := verification.CallResult{CallID: call.ID, Report: &reports[0]}
			switch mode {
			case "wrong report":
				result.Report = &reports[1]
				_, err = cs.CompleteVerificationCall(ctx, lease, result)
				require.ErrorIs(t, err, verification.ErrInvalid)
				saved, readErr := js.GetVerificationJob(ctx, req.ID)
				require.NoError(t, readErr)
				require.Equal(t, job, saved)
			case "failed evaluator":
				result.Report, result.Failure = nil, "provider timeout"
				job, err = cs.CompleteVerificationCall(ctx, lease, result)
				require.NoError(t, err)
				require.Equal(t, verification.JobInconclusive, job.State)
				require.Nil(t, job.Lease)
				require.Empty(t, job.ReportIDs)
				require.Equal(t, verification.EvaluationDebit{Calls: 1, Tokens: 40, USDMicros: 40}, verification.DebitCalls(job.Calls))
			case "overrun":
				result.Usage = verification.EvaluationUsage{Tokens: 101, TokensKnown: true, USDMicros: 101, MoneyKnown: true, Receipt: "provider-overrun"}
				job, err = cs.CompleteVerificationCall(ctx, lease, result)
				require.NoError(t, err)
				require.Equal(t, int64(101), verification.DebitCalls(job.Calls).Tokens)
				_, err = cs.BeginVerificationCall(ctx, lease, callFor(reports[1], 0, 0))
				require.ErrorIs(t, err, verification.ErrJobBudgetExceeded)
			case "changed receipt":
				job, err = cs.CompleteVerificationCall(ctx, lease, result)
				require.NoError(t, err)
				result.Usage = verification.EvaluationUsage{TokensKnown: true, Receipt: "different-receipt"}
				_, err = cs.CompleteVerificationCall(ctx, lease, result)
				require.ErrorIs(t, err, verification.ErrConflict)
				saved, readErr := js.GetVerificationJob(ctx, req.ID)
				require.NoError(t, readErr)
				require.Equal(t, job, saved)
			}
		})
	}
}

// VerificationCallRecovery preserves dispatch across takeover and transaction failure.
func VerificationCallRecovery(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	ctx, req, reports := verificationCallFixture(t, s, false)
	clock := &jobClock{at: time.Now().UTC()}
	repository := assurance.New(driver).WithClock(clock)
	job := claimCallJob(ctx, t, repository, req)
	oldLease := *job.Lease
	call := callFor(reports[0], 40, 40)
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	failure := errors.New("interrupted dispatch transaction")
	failed := assurance.New(failingRunDriver{Driver: driver, transaction: transaction, failure: failure}).WithClock(clock)
	_, err := failed.BeginVerificationCall(ctx, oldLease, call)
	require.ErrorIs(t, err, failure)
	saved, err := repository.GetVerificationJob(ctx, req.ID)
	require.NoError(t, err)
	require.Equal(t, job, saved)
	_, err = repository.BeginVerificationCall(ctx, oldLease, call)
	require.NoError(t, err)
	clock.advance(61 * time.Second)
	job, err = repository.ClaimVerificationJob(ctx, verification.JobClaim{JobID: req.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "replacement", Duration: time.Minute})
	require.NoError(t, err)
	require.Equal(t, int64(1), job.Calls[0].Generation)
	_, err = repository.BeginVerificationCall(ctx, *job.Lease, call)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	replayed := call
	replayed.ID = id.New(verification.PrefixCall)
	_, err = repository.BeginVerificationCall(ctx, *job.Lease, replayed)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	result := verification.CallResult{CallID: call.ID, Report: &reports[0], Usage: verification.EvaluationUsage{Tokens: 30, TokensKnown: true, Receipt: "retained-meter-receipt"}}
	_, err = repository.CompleteVerificationCall(ctx, oldLease, result)
	require.ErrorIs(t, err, verification.ErrJobConflict)
	_, err = failed.CompleteVerificationCall(ctx, *job.Lease, result)
	require.ErrorIs(t, err, failure)
	saved, err = repository.GetVerificationJob(ctx, req.ID)
	require.NoError(t, err)
	require.Equal(t, job, saved)
	_, err = repository.GetReport(ctx, reports[0].ID)
	require.ErrorIs(t, err, verification.ErrNotFound)
	job, err = repository.CompleteVerificationCall(ctx, *job.Lease, result)
	require.NoError(t, err)
	require.Len(t, job.ReportIDs, 1)
	require.Equal(t, int64(30), verification.DebitCalls(job.Calls).Tokens)
}

// VerificationCallRestart proves a lost dispatch acknowledgement is not replayable.
func VerificationCallRestart(t *testing.T, s store.Store, reopen func() store.Store) {
	t.Helper()
	ctx, req, reports := verificationCallFixture(t, s, false)
	job := claimCallJob(ctx, t, resultCapability[verification.JobStore](t, s), req)
	call := callFor(reports[0], 40, 40)
	job, err := resultCapability[verification.CallStore](t, s).BeginVerificationCall(ctx, *job.Lease, call)
	require.NoError(t, err)
	restored := reopen()
	js := resultCapability[verification.JobStore](t, restored)
	cs := resultCapability[verification.CallStore](t, restored)
	saved, err := js.GetVerificationJob(ctx, req.ID)
	require.NoError(t, err)
	require.Equal(t, job, saved)
	_, err = cs.BeginVerificationCall(ctx, *job.Lease, call)
	require.ErrorIs(t, err, verification.ErrCallUncertain)
	_, err = cs.CompleteVerificationCall(ctx, *job.Lease, verification.CallResult{CallID: call.ID, Report: &reports[0]})
	require.NoError(t, err)
}
