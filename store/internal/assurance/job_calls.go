package assurance

import (
	"context"
	"slices"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// BeginVerificationCall records a dispatch and reserves its allowance together.
func (s *Repository) BeginVerificationCall(ctx context.Context, lease verification.JobLease, request verification.CallRequest) (verification.Job, error) {
	if lease.Validate() != nil || request.Validate() != nil {
		return verification.Job{}, verification.ErrInvalid
	}
	return s.mutateJob(ctx, lease.JobID, func(txctx context.Context, repository *Repository, tx RunTransaction, job *verification.Job, now time.Time) error {
		if err := job.CheckLease(lease, now); err != nil {
			return err
		}
		if err := checkJobRun(txctx, tx, *job); err != nil {
			return err
		}
		if job.Request.EvaluationBudget == nil || job.State != verification.JobRunning {
			return verification.ErrJobConflict
		}
		for _, call := range job.Calls {
			if call.Request.ID == request.ID || (call.Request.Verifier == request.Verifier && call.Request.VerifierRevision == request.VerifierRevision) {
				return verification.ErrCallUncertain
			}
		}
		if err := job.Request.EvaluationBudget.Admit(verification.DebitCalls(job.Calls), request); err != nil {
			return err
		}
		contract, err := repository.GetContract(txctx, job.Request.ContractID)
		if err != nil {
			return err
		}
		criteria := make([]string, 0, len(contract.Criteria))
		for _, criterion := range contract.Criteria {
			if criterion.Verifier == request.Verifier && criterion.VerifierRevision == request.VerifierRevision {
				criteria = append(criteria, criterion.ID)
			}
		}
		if len(criteria) == 0 {
			return verification.ErrInvalid
		}
		job.Calls = append(job.Calls, verification.EvaluationCall{Request: request, Generation: lease.Generation, CriterionIDs: criteria, StartedAt: now})
		return nil
	})
}

// CompleteVerificationCall saves usage, report and job progress atomically.
func (s *Repository) CompleteVerificationCall(ctx context.Context, lease verification.JobLease, result verification.CallResult) (verification.Job, error) {
	if lease.Validate() != nil {
		return verification.Job{}, verification.ErrInvalid
	}
	frozen, completion, err := result.Freeze()
	if err != nil {
		return verification.Job{}, err
	}
	return s.mutateJob(ctx, lease.JobID, func(txctx context.Context, repository *Repository, tx RunTransaction, job *verification.Job, now time.Time) error {
		if err := job.CheckLease(lease, now); err != nil {
			return err
		}
		if err := checkJobRun(txctx, tx, *job); err != nil {
			return err
		}
		if job.Request.EvaluationBudget == nil {
			return verification.ErrJobConflict
		}
		index := slices.IndexFunc(job.Calls, func(call verification.EvaluationCall) bool { return call.Request.ID == frozen.CallID })
		if index < 0 {
			return verification.ErrJobConflict
		}
		call := &job.Calls[index]
		if call.Completion != nil {
			if *call.Completion != completion {
				return verification.ErrConflict
			}
			return nil
		}
		if now.Before(call.StartedAt) {
			return verification.ErrInvalid
		}
		if frozen.Report != nil {
			if len(frozen.Report.Outcomes) != len(call.CriterionIDs) {
				return verification.ErrInvalid
			}
			for _, outcome := range frozen.Report.Outcomes {
				if !slices.Contains(call.CriterionIDs, outcome.CriterionID) || outcome.Verifier != call.Request.Verifier || outcome.VerifierRevision != call.Request.VerifierRevision {
					return verification.ErrInvalid
				}
			}
		}
		call.Completion, call.CompletedAt = &completion, now
		if frozen.Report == nil {
			job.State, job.Lease = verification.JobInconclusive, nil
			return tx.SetRunVerification(txctx, job.Request.Binding.RunID, job.RunRevision, verification.StatusInconclusive, id.ID{}, now)
		}
		return repository.acceptJobReport(txctx, tx, job, *frozen.Report, now)
	})
}
