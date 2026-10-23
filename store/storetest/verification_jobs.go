package storetest

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/store/internal/assurance"
	"github.com/xraph/cortex/verification"
)

type jobClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *jobClock) Now() time.Time          { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *jobClock) advance(d time.Duration) { c.mu.Lock(); defer c.mu.Unlock(); c.at = c.at.Add(d) }

func verificationJobFixture(t *testing.T, s store.Store) (context.Context, verification.JobRequest, []verification.Report) {
	t.Helper()
	ctx, original := resultFixture(t, s)
	vs := resultCapability[verification.Store](t, s)
	contract, err := vs.GetContract(ctx, original.ContractID)
	require.NoError(t, err)
	contract.ID = id.New(verification.PrefixContract)
	second := contract.Criteria[0]
	second.ID = "second"
	contract.Criteria = append(contract.Criteria, second)
	contract, err = verification.SealContract(contract)
	require.NoError(t, err)
	require.NoError(t, vs.CreateContract(ctx, contract))
	current, err := s.GetRun(ctx, original.Current.RunID)
	require.NoError(t, err)
	current.VerificationStatus = verification.StatusUnchecked
	current.Provenance.AcceptanceRef = contract.ID.String()
	require.NoError(t, s.UpdateRun(ctx, current))
	binding := original.Current
	binding.ContractDigest = contract.Digest
	req := verification.JobRequest{SchemaVersion: 1, ID: id.New(verification.PrefixJob), ExpectedRunRevision: current.Revision, ContractID: contract.ID, Binding: binding, Deadline: time.Now().UTC().Add(5 * time.Minute), MaxAttempts: 3}
	reports := make([]verification.Report, 0, 2)
	for _, criterion := range contract.Criteria {
		report, readErr := vs.GetReport(ctx, original.ReportIDs[0])
		require.NoError(t, readErr)
		report.ID = id.New(verification.PrefixReport)
		report.Binding = binding
		report.Outcomes[0].CriterionID = criterion.ID
		report, readErr = verification.SealReport(report)
		require.NoError(t, readErr)
		reports = append(reports, report)
	}
	return ctx, req, reports
}

func jobCommitRequest(job verification.Job) verification.CommitRequest {
	return verification.CommitRequest{SchemaVersion: 1, ResultID: id.New(verification.PrefixResult), ExpectedRunRevision: job.RunRevision, ContractID: job.Request.ContractID, ReportIDs: append([]id.ID(nil), job.ReportIDs...), Current: job.Request.Binding, JobLease: job.Lease}
}

func testVerificationJobs(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	js := resultCapability[verification.JobStore](t, s)
	ctx, req, reports := verificationJobFixture(t, s)
	job, err := js.EnqueueVerificationJob(ctx, req)
	require.NoError(t, err)
	require.NoError(t, job.Validate())
	require.Equal(t, verification.JobQueued, job.State)
	require.Equal(t, req.ExpectedRunRevision+1, job.RunRevision)
	retry, err := js.EnqueueVerificationJob(ctx, req)
	require.NoError(t, err)
	require.Equal(t, job, retry)
	changed := req
	changed.MaxAttempts++
	_, err = js.EnqueueVerificationJob(ctx, changed)
	require.ErrorIs(t, err, verification.ErrJobConflict)
	claim := verification.JobClaim{JobID: req.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker", Duration: time.Minute}
	job, err = js.ClaimVerificationJob(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, int64(1), job.Generation)
	retry, err = js.ClaimVerificationJob(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, job, retry)
	job, err = js.SaveVerificationJobReport(ctx, *job.Lease, reports[0])
	require.NoError(t, err)
	require.Equal(t, verification.JobRunning, job.State)
	retry, err = js.SaveVerificationJobReport(ctx, *job.Lease, reports[0])
	require.NoError(t, err)
	require.Equal(t, job, retry)
	job, err = js.SaveVerificationJobReport(ctx, *job.Lease, reports[1])
	require.NoError(t, err)
	require.Equal(t, verification.JobReady, job.State)
	cs := resultCapability[verification.CommitStore](t, s)
	commit := jobCommitRequest(job)
	noLease := commit
	noLease.JobLease = nil
	_, err = cs.CommitVerifiedResult(ctx, noLease)
	require.ErrorIs(t, err, verification.ErrJobConflict)
	receipt, err := cs.CommitVerifiedResult(ctx, commit)
	require.NoError(t, err)
	saved, err := js.GetVerificationJob(ctx, req.ID)
	require.NoError(t, err)
	require.Equal(t, verification.JobCommitted, saved.State)
	require.Equal(t, receipt.Request.ResultID, saved.ResultID)
	require.Nil(t, saved.Lease)
	repeated, err := cs.CommitVerifiedResult(ctx, commit)
	require.NoError(t, err)
	require.Equal(t, receipt, repeated)
	listed, err := js.ListVerificationJobs(ctx, verification.JobListRequest{RunID: req.Binding.RunID, Limit: 1})
	require.NoError(t, err)
	require.Equal(t, []verification.Job{saved}, listed)
	listed, err = js.ListVerificationJobs(ctx, verification.JobListRequest{After: req.ID, Limit: 1})
	require.NoError(t, err)
	require.Empty(t, listed)
	_, err = js.GetVerificationJob(ctxWithScope("other"), req.ID)
	require.ErrorIs(t, err, verification.ErrJobNotFound)
	_, err = js.ClaimVerificationJob(context.Background(), claim)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = js.EnqueueVerificationJob(cancelled, req)
	require.ErrorIs(t, err, context.Canceled)
}

// VerificationJobLeases exercises persisted ownership using deterministic host time.
func VerificationJobLeases(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	ctx, req, reports := verificationJobFixture(t, s)
	clock := &jobClock{at: time.Now().UTC()}
	repository := assurance.New(driver).WithClock(clock)
	_, err := repository.EnqueueVerificationJob(ctx, req)
	require.NoError(t, err)
	job, err := repository.ClaimVerificationJob(ctx, verification.JobClaim{JobID: req.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "old", Duration: 10 * time.Second})
	require.NoError(t, err)
	old := *job.Lease
	job, err = repository.SaveVerificationJobReport(ctx, old, reports[0])
	require.NoError(t, err)
	clock.advance(11 * time.Second)
	_, err = repository.RenewVerificationJob(ctx, old, time.Minute)
	require.ErrorIs(t, err, verification.ErrJobExpired)
	forged := old
	forged.ExpiresAt = clock.Now().Add(time.Hour)
	_, err = repository.RenewVerificationJob(ctx, forged, time.Minute)
	require.ErrorIs(t, err, verification.ErrJobExpired)
	// A new repository instance resumes saved progress without evaluating it again.
	repository = assurance.New(driver).WithClock(clock)
	job, err = repository.ClaimVerificationJob(ctx, verification.JobClaim{JobID: req.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "new", Duration: 10 * time.Second})
	require.NoError(t, err)
	require.Equal(t, int64(2), job.Generation)
	require.Equal(t, []id.ID{reports[0].ID}, job.ReportIDs)
	_, err = repository.SaveVerificationJobReport(ctx, old, reports[1])
	require.ErrorIs(t, err, verification.ErrJobConflict)
	_, err = repository.GetReport(ctx, reports[1].ID)
	require.ErrorIs(t, err, verification.ErrNotFound)
	current := *job.Lease
	clock.advance(time.Second)
	job, err = repository.RenewVerificationJob(ctx, current, time.Minute)
	require.NoError(t, err)
	require.True(t, job.Lease.ExpiresAt.After(current.ExpiresAt))
	require.Equal(t, current.Generation, job.Lease.Generation)
	job, err = repository.SaveVerificationJobReport(ctx, *job.Lease, reports[1])
	require.NoError(t, err)
	commit := jobCommitRequest(job)
	stale := commit
	stale.JobLease = &old
	_, err = repository.CommitVerifiedResult(ctx, stale)
	require.ErrorIs(t, err, verification.ErrJobConflict)
	_, err = repository.GetCommittedResult(ctx, commit.ResultID)
	require.ErrorIs(t, err, verification.ErrNotFound)
	_, err = repository.CommitVerifiedResult(ctx, commit)
	require.NoError(t, err)
	// A replayed old claim must not create a new generation after a later lease.
	replayCtx, replayReq, _ := verificationJobFixture(t, s)
	_, err = repository.EnqueueVerificationJob(replayCtx, replayReq)
	require.NoError(t, err)
	firstClaim := verification.JobClaim{JobID: replayReq.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "first", Duration: time.Second}
	_, err = repository.ClaimVerificationJob(replayCtx, firstClaim)
	require.NoError(t, err)
	clock.advance(2 * time.Second)
	_, err = repository.ClaimVerificationJob(replayCtx, verification.JobClaim{JobID: replayReq.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "second", Duration: time.Second})
	require.NoError(t, err)
	clock.advance(2 * time.Second)
	_, err = repository.ClaimVerificationJob(replayCtx, firstClaim)
	require.ErrorIs(t, err, verification.ErrJobConflict)
}

// VerificationJobTermination checks deadlines, attempt limits, cancellation and
// failed checks without assigning a successful verification status to the run.
func VerificationJobTermination(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	for _, mode := range []string{"deadline", "attempts", "cancel", "failed", "inconclusive", "missing proof", "missing human", "missing sources", "expired accepted report"} {
		t.Run(mode, func(t *testing.T) {
			ctx, req, reports := verificationJobFixture(t, s)
			policyCase := mode == "missing proof" || mode == "missing human" || mode == "missing sources" || mode == "expired accepted report"
			if policyCase {
				vs := resultCapability[verification.Store](t, s)
				contract, err := vs.GetContract(ctx, req.ContractID)
				require.NoError(t, err)
				contract.ID = id.New(verification.PrefixContract)
				switch mode {
				case "missing proof":
					contract.Criteria[0].MinEvidence = 1
				case "missing human":
					contract.Criteria[0].Kind = verification.KindHuman
				case "missing sources":
					contract.Criteria[0].MaxSourceAge = time.Minute
				case "expired accepted report":
					contract.Criteria[0].MaxAge = time.Second
				}
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
				for i := range reports {
					reports[i].Binding = req.Binding
					if mode == "expired accepted report" {
						reports[i].CheckedAt = time.Now().UTC()
					}
					reports[i], err = verification.SealReport(reports[i])
					require.NoError(t, err)
				}
			}
			req.MaxAttempts = 1
			clock := &jobClock{at: time.Now().UTC()}
			repository := assurance.New(driver).WithClock(clock)
			_, err := repository.EnqueueVerificationJob(ctx, req)
			require.NoError(t, err)
			claim := verification.JobClaim{JobID: req.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker", Duration: time.Second}
			if mode == "expired accepted report" {
				claim.Duration = time.Minute
			}
			job, err := repository.ClaimVerificationJob(ctx, claim)
			require.NoError(t, err)
			lease := *job.Lease
			switch mode {
			case "deadline":
				clock.advance(6 * time.Minute)
				job, err = repository.ExpireVerificationJob(ctx, req.ID)
			case "attempts":
				clock.advance(2 * time.Second)
				claim.ClaimID = id.New(verification.PrefixJobClaim)
				_, err = repository.ClaimVerificationJob(ctx, claim)
				require.ErrorIs(t, err, verification.ErrJobExpired)
				job, err = repository.ExpireVerificationJob(ctx, req.ID)
			case "cancel":
				job, err = repository.CancelVerificationJob(ctx, req.ID)
			case "expired accepted report":
				job, err = repository.SaveVerificationJobReport(ctx, lease, reports[0])
				require.NoError(t, err)
				require.Equal(t, verification.JobRunning, job.State)
				clock.advance(2 * time.Second)
				job, err = repository.SaveVerificationJobReport(ctx, lease, reports[1])
			default:
				if !policyCase {
					reports[0].Outcomes[0].Status = verification.StatusFailed
				}
				if mode == "inconclusive" {
					reports[0].Outcomes[0].Status = verification.StatusInconclusive
				}
				reports[0], err = verification.SealReport(reports[0])
				require.NoError(t, err)
				job, err = repository.SaveVerificationJobReport(ctx, lease, reports[0])
			}
			require.NoError(t, err)
			if policyCase {
				require.Equal(t, verification.JobInconclusive, job.State)
			}
			require.Nil(t, job.Lease)
			require.NotEqual(t, verification.JobCommitted, job.State)
			_, err = repository.SaveVerificationJobReport(ctx, lease, reports[1])
			require.ErrorIs(t, err, verification.ErrJobConflict)
			current, err := s.GetRun(ctx, req.Binding.RunID)
			require.NoError(t, err)
			require.NotEqual(t, verification.StatusPassed, current.VerificationStatus)
			if mode == "expired accepted report" {
				require.Equal(t, verification.StatusStale, current.VerificationStatus)
			} else if policyCase {
				require.Equal(t, verification.StatusInconclusive, current.VerificationStatus)
			}
			require.Equal(t, job.RunRevision+1, current.Revision)
		})
	}
}

// VerificationJobConcurrentClaims permits one worker generation per live lease.
func VerificationJobConcurrentClaims(t *testing.T, s store.Store) {
	t.Helper()
	ctx, req, _ := verificationJobFixture(t, s)
	js := resultCapability[verification.JobStore](t, s)
	_, err := js.EnqueueVerificationJob(ctx, req)
	require.NoError(t, err)
	results := make(chan error, 4)
	start := make(chan struct{})
	for range 4 {
		go func() {
			<-start
			_, claimErr := js.ClaimVerificationJob(ctx, verification.JobClaim{JobID: req.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker", Duration: time.Minute})
			results <- claimErr
		}()
	}
	close(start)
	winners := 0
	for range 4 {
		if claimErr := <-results; claimErr == nil {
			winners++
		} else {
			require.ErrorIs(t, claimErr, verification.ErrJobConflict)
		}
	}
	require.Equal(t, 1, winners)
	// A superseded job may be cancelled without altering its replacement run.
	before, err := s.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	before.VerificationStatus = verification.StatusUnchecked
	require.NoError(t, s.UpdateRun(ctx, before))
	next := req
	next.ID = id.New(verification.PrefixJob)
	next.ExpectedRunRevision = before.Revision
	_, err = js.EnqueueVerificationJob(ctx, next)
	require.NoError(t, err)
	before, err = s.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	_, err = js.CancelVerificationJob(ctx, req.ID)
	require.NoError(t, err)
	after, err := s.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, before, after)
}

// VerificationJobRollback injects failure after enqueue, report and final commit
// writes, then proves each operation can be retried without partial progress.
func VerificationJobRollback(t *testing.T, s store.Store, driver assurance.Driver) {
	t.Helper()
	transaction, ok := driver.(assurance.RunDriver)
	require.True(t, ok)
	failure := errors.New("verification job transaction interrupted")
	failed := assurance.New(failingRunDriver{Driver: driver, transaction: transaction, failure: failure})
	repository := assurance.New(driver)
	ctx, req, reports := verificationJobFixture(t, s)
	before, err := s.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	_, err = failed.EnqueueVerificationJob(ctx, req)
	require.ErrorIs(t, err, failure)
	_, err = repository.GetVerificationJob(ctx, req.ID)
	require.ErrorIs(t, err, verification.ErrJobNotFound)
	after, err := s.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, err = repository.EnqueueVerificationJob(ctx, req)
	require.NoError(t, err)
	job, err := repository.ClaimVerificationJob(ctx, verification.JobClaim{JobID: req.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker", Duration: time.Minute})
	require.NoError(t, err)
	_, err = failed.SaveVerificationJobReport(ctx, *job.Lease, reports[0])
	require.ErrorIs(t, err, failure)
	_, err = repository.GetReport(ctx, reports[0].ID)
	require.ErrorIs(t, err, verification.ErrNotFound)
	saved, err := repository.GetVerificationJob(ctx, req.ID)
	require.NoError(t, err)
	require.Equal(t, job, saved)
	for _, report := range reports {
		job, err = repository.SaveVerificationJobReport(ctx, *job.Lease, report)
		require.NoError(t, err)
	}
	before, err = s.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	commit := jobCommitRequest(job)
	receipt, err := failed.CommitVerifiedResult(ctx, commit)
	require.ErrorIs(t, err, failure)
	require.Zero(t, receipt)
	_, err = repository.GetCommittedResult(ctx, commit.ResultID)
	require.ErrorIs(t, err, verification.ErrNotFound)
	saved, err = repository.GetVerificationJob(ctx, req.ID)
	require.NoError(t, err)
	require.Equal(t, job, saved)
	after, err = s.GetRun(ctx, req.Binding.RunID)
	require.NoError(t, err)
	require.Equal(t, before, after)
	_, err = repository.CommitVerifiedResult(ctx, commit)
	require.NoError(t, err)
}

// VerificationJobRestart preserves partial reports and claim identity after
// closing and reopening the database. It never invokes generation or tools.
func VerificationJobRestart(t *testing.T, s store.Store, reopen func() store.Store) {
	t.Helper()
	ctx, req, reports := verificationJobFixture(t, s)
	js := resultCapability[verification.JobStore](t, s)
	_, err := js.EnqueueVerificationJob(ctx, req)
	require.NoError(t, err)
	claim := verification.JobClaim{JobID: req.ID, ClaimID: id.New(verification.PrefixJobClaim), Owner: "worker", Duration: time.Minute}
	job, err := js.ClaimVerificationJob(ctx, claim)
	require.NoError(t, err)
	job, err = js.SaveVerificationJobReport(ctx, *job.Lease, reports[0])
	require.NoError(t, err)
	restored := reopen()
	js = resultCapability[verification.JobStore](t, restored)
	saved, err := js.GetVerificationJob(ctx, req.ID)
	require.NoError(t, err)
	require.Equal(t, job, saved)
	retry, err := js.ClaimVerificationJob(ctx, claim)
	require.NoError(t, err)
	require.Equal(t, job, retry)
	job, err = js.SaveVerificationJobReport(ctx, *retry.Lease, reports[1])
	require.NoError(t, err)
	_, err = resultCapability[verification.CommitStore](t, restored).CommitVerifiedResult(ctx, jobCommitRequest(job))
	require.NoError(t, err)
}
