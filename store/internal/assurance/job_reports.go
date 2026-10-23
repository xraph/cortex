package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// SaveVerificationJobReport atomically records a leased worker's report and
// progress. Accepted criteria cannot be overwritten by a more favorable retry.
func (s *Repository) SaveVerificationJobReport(ctx context.Context, lease verification.JobLease, report verification.Report) (verification.Job, error) {
	if lease.Validate() != nil {
		return verification.Job{}, verification.ErrInvalid
	}
	if err := report.Validate(); err != nil {
		return verification.Job{}, err
	}
	frozen, err := verification.SealReport(report)
	if err != nil {
		return verification.Job{}, err
	}
	return s.mutateJob(ctx, lease.JobID, func(txctx context.Context, repository *Repository, tx RunTransaction, job *verification.Job, now time.Time) error {
		if leaseErr := job.CheckLease(lease, now); leaseErr != nil {
			return leaseErr
		}
		if runErr := repository.checkJobRun(txctx, tx, *job); runErr != nil {
			return runErr
		}
		if job.Request.EvaluationBudget != nil {
			return verification.ErrJobConflict
		}
		return repository.acceptJobReport(txctx, tx, job, frozen, now)
	})
}

func (s *Repository) acceptJobReport(txctx context.Context, tx RunTransaction, job *verification.Job, frozen verification.Report, now time.Time) error {
	if verification.CheckFreshness(frozen, job.Request.Binding, now) != verification.StatusPassed {
		return verification.ErrConflict
	}
	contract, readErr := s.GetContract(txctx, job.Request.ContractID)
	if readErr != nil {
		return readErr
	}
	if checkErr := verification.ValidateReportForContract(contract, frozen); checkErr != nil {
		return checkErr
	}
	existing, readErr := s.GetReport(txctx, frozen.ID)
	if readErr != nil && !errors.Is(readErr, verification.ErrNotFound) {
		return readErr
	}
	if readErr == nil && existing.Digest != frozen.Digest {
		return verification.ErrConflict
	}
	if slices.Contains(job.ReportIDs, frozen.ID) {
		return nil
	}
	if len(job.ReportIDs) >= 256 {
		return verification.ErrInvalid
	}
	reports, loadErr := s.jobReports(txctx, *job)
	if loadErr != nil {
		return loadErr
	}
	seen := make(map[string]bool)
	for _, saved := range reports {
		for _, outcome := range saved.Outcomes {
			seen[outcome.CriterionID] = true
		}
	}
	for _, outcome := range frozen.Outcomes {
		if seen[outcome.CriterionID] {
			return verification.ErrConflict
		}
	}
	reports = append(reports, frozen)
	if boundsErr := reportBounds(reports); boundsErr != nil {
		return boundsErr
	}
	if errors.Is(readErr, verification.ErrNotFound) {
		if saveErr := s.SaveReport(txctx, frozen); saveErr != nil {
			return saveErr
		}
	}
	job.ReportIDs = append(job.ReportIDs, frozen.ID)
	for _, outcome := range frozen.Outcomes {
		seen[outcome.CriterionID] = true
	}
	decision := verification.Aggregate(contract, reports, now)
	status := verification.StatusPending
	for _, criterion := range decision.Criteria {
		if !criterion.Required || !seen[criterion.CriterionID] {
			continue
		}
		if criterion.Status == verification.StatusFailed {
			status = verification.StatusFailed
			break
		}
		if criterion.Status == verification.StatusStale {
			status = verification.StatusStale
		} else if criterion.Status == verification.StatusInconclusive && status != verification.StatusStale {
			status = verification.StatusInconclusive
		}
	}
	if status != verification.StatusPending {
		job.State = verification.JobInconclusive
		if status == verification.StatusFailed {
			job.State = verification.JobFailed
		}
		job.Lease = nil
		return s.setVerificationWithEvent(txctx, tx, job.Request.Binding.RunID, job.RunRevision, status, now)
	}
	if decision.Status == verification.StatusPassed && !slices.ContainsFunc(job.Calls, func(call verification.EvaluationCall) bool { return call.Completion == nil }) {
		job.State = verification.JobReady
	}
	return nil
}

func (s *Repository) jobReports(ctx context.Context, job verification.Job) ([]verification.Report, error) {
	reports := make([]verification.Report, 0, len(job.ReportIDs))
	total := 0
	for _, reportID := range job.ReportIDs {
		report, err := s.GetReport(ctx, reportID)
		if err != nil {
			return nil, err
		}
		reports = append(reports, report)
		total, err = addReportSize(total, report)
		if err != nil {
			return nil, err
		}
	}
	return reports, nil
}

func addReportSize(total int, report verification.Report) (int, error) {
	data, err := json.Marshal(report)
	if err != nil {
		return total, err
	}
	total += len(data)
	if total > 8<<20 {
		return total, verification.ErrInvalid
	}
	return total, nil
}

func reportBounds(reports []verification.Report) error {
	total := 0
	for _, report := range reports {
		var err error
		total, err = addReportSize(total, report)
		if err != nil {
			return err
		}
	}
	return nil
}

// ExpireVerificationJob ends overdue or exhausted work without claiming it.
func (s *Repository) ExpireVerificationJob(ctx context.Context, jobID id.ID) (verification.Job, error) {
	return s.finishJob(ctx, jobID, false)
}

// CancelVerificationJob revokes unfinished work. It never retracts a commit.
func (s *Repository) CancelVerificationJob(ctx context.Context, jobID id.ID) (verification.Job, error) {
	return s.finishJob(ctx, jobID, true)
}

func (s *Repository) finishJob(ctx context.Context, jobID id.ID, cancel bool) (verification.Job, error) {
	return s.mutateJob(ctx, jobID, func(txctx context.Context, repository *Repository, tx RunTransaction, job *verification.Job, now time.Time) error {
		target := verification.JobInconclusive
		if cancel {
			target = verification.JobCancelled
		}
		if job.State == target {
			return nil
		}
		if job.State != verification.JobQueued && job.State != verification.JobRunning && job.State != verification.JobReady {
			return verification.ErrJobConflict
		}
		if !cancel && job.Request.Deadline.After(now) && (job.Attempts < job.Request.MaxAttempts || (job.Lease != nil && job.Lease.ExpiresAt.After(now))) {
			return verification.ErrJobConflict
		}
		current, err := tx.GetRun(txctx, job.Request.Binding.RunID)
		if err != nil {
			return err
		}
		// Retire stale jobs without changing a newer candidate or verification attempt.
		if current.Revision == job.RunRevision && current.VerificationStatus == verification.StatusPending {
			if updateErr := repository.setVerificationWithEvent(txctx, tx, current.ID, current.Revision, verification.StatusInconclusive, now); updateErr != nil {
				return updateErr
			}
		}
		job.State = target
		job.Lease = nil
		return nil
	})
}

func (s *Repository) jobForCommit(ctx context.Context, req verification.CommitRequest, now time.Time) (*verification.Job, Record, error) {
	index, err := s.get(ctx, Query{Kind: kindJobBinding, ID: jobBindingKey(req.Current.RunID, req.ExpectedRunRevision)})
	if errors.Is(err, ErrAbsent) {
		if req.JobLease != nil {
			return nil, Record{}, verification.ErrJobConflict
		}
		return nil, Record{}, nil //nolint:nilnil // A legacy manual commit has no job binding.
	}
	if err != nil {
		return nil, Record{}, err
	}
	var jobID id.ID
	if index.Deleted || index.RunID != req.Current.RunID.String() || json.Unmarshal([]byte(index.Body), &jobID) != nil {
		return nil, Record{}, verification.ErrInvalid
	}
	job, row, err := s.jobRecord(ctx, jobID)
	if err != nil {
		return nil, Record{}, err
	}
	if req.JobLease == nil || job.RequestDigest != index.Digest || job.RunRevision != req.ExpectedRunRevision || job.Request.ContractID != req.ContractID || job.State != verification.JobReady || !slices.Equal(job.ReportIDs, req.ReportIDs) {
		return nil, Record{}, verification.ErrJobConflict
	}
	if leaseErr := job.CheckLease(*req.JobLease, now); leaseErr != nil {
		return nil, Record{}, leaseErr
	}
	actual, err := job.Request.Binding.Digest()
	if err != nil {
		return nil, Record{}, err
	}
	expected, err := req.Current.Digest()
	if err != nil || actual != expected {
		return nil, Record{}, verification.ErrJobConflict
	}
	return &job, row, nil
}

func commitJob(ctx context.Context, tx RunTransaction, job *verification.Job, previous Record, resultID id.ID, now time.Time) error {
	if job == nil {
		return nil
	}
	if job.Revision == math.MaxInt64 {
		return verification.ErrJobConflict
	}
	job.State = verification.JobCommitted
	job.ResultID = resultID
	job.Lease = nil
	job.Revision++
	job.UpdatedAt = now
	sealed, err := verification.SealJob(*job)
	if err != nil {
		return err
	}
	row, err := record(KindJob, job.Request.ID.String(), sealed.Digest, job.Request.Binding.RunID.String(), job.Request.Binding.Scope, sealed)
	if err != nil {
		return err
	}
	return tx.ReplaceJob(ctx, row, previous.Body)
}
