package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

// KindJob is the only assurance record whose body job operations may replace.
const KindJob = "verification_job"
const kindJobBinding = "verification_job_binding"

func jobBindingKey(runID id.AgentRunID, revision int64) string {
	return fmt.Sprintf("%s:%020d", runID.String(), revision)
}

func (s *Repository) jobRecord(ctx context.Context, jobID id.ID) (verification.Job, Record, error) {
	if jobID.IsNil() || jobID.Prefix() != verification.PrefixJob {
		return verification.Job{}, Record{}, verification.ErrInvalid
	}
	row, err := s.get(ctx, Query{Kind: KindJob, ID: jobID.String()})
	if errors.Is(err, ErrAbsent) {
		return verification.Job{}, Record{}, verification.ErrJobNotFound
	}
	if err != nil {
		return verification.Job{}, Record{}, err
	}
	job, err := decodeJob(row)
	return job, row, err
}

func decodeJob(row Record) (verification.Job, error) {
	if row.Deleted || len(row.Body) > 1<<20 {
		return verification.Job{}, verification.ErrInvalid
	}
	var job verification.Job
	if err := json.Unmarshal([]byte(row.Body), &job); err != nil {
		return verification.Job{}, verification.ErrInvalid
	}
	if err := job.Validate(); err != nil {
		return verification.Job{}, err
	}
	if row.Kind != KindJob || row.ID != job.Request.ID.String() || row.Digest != job.Digest || row.RunID != job.Request.Binding.RunID.String() || row.ScopeCanon != job.Request.Binding.Scope.Canonical() {
		return verification.Job{}, verification.ErrInvalid
	}
	return job, nil
}

// GetVerificationJob returns one exact-scope durable work record.
func (s *Repository) GetVerificationJob(ctx context.Context, jobID id.ID) (verification.Job, error) {
	job, _, err := s.jobRecord(ctx, jobID)
	return job, err
}

// ListVerificationJobs pages work records without granting cross-scope discovery.
func (s *Repository) ListVerificationJobs(ctx context.Context, req verification.JobListRequest) ([]verification.Job, error) {
	if req.Limit < 1 || req.Limit > 256 || (!req.RunID.IsNil() && req.RunID.Prefix() != id.PrefixAgentRun) || (!req.After.IsNil() && req.After.Prefix() != verification.PrefixJob) {
		return nil, verification.ErrInvalid
	}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return nil, err
	}
	rows, err := s.driver.Select(ctx, Query{Kind: KindJob, RunID: req.RunID.String(), After: req.After.String(), Limit: req.Limit})
	if err != nil {
		return nil, err
	}
	jobs := make([]verification.Job, 0, len(rows))
	for _, row := range rows {
		if checkErr := validateRecord(ctx, row); checkErr != nil {
			return nil, checkErr
		}
		job, decodeErr := decodeJob(row)
		if decodeErr != nil {
			return nil, decodeErr
		}
		jobs = append(jobs, job)
	}
	return jobs, nil
}

func validateCapturedRun(r *run.Run, expected int64, contractID id.ID, binding verification.Binding) error {
	if r.Scope.Canonical() != binding.Scope.Canonical() {
		return cortex.ErrRunNotFound
	}
	if r.ID != binding.RunID || r.Revision != expected || r.State != run.StateCompleted || r.StopReason != run.StopFinalAnswer || r.ResultRef != "" || r.Provenance.PreparedDigest != binding.Context.PreparedDigest || r.Provenance.AcceptanceRef != contractID.String() {
		return cortex.ErrRunConflict
	}
	if int64(len(r.Output)) != binding.Candidate.Bytes || evidence.Digest([]byte(r.Output)) != binding.Candidate.SHA256 {
		return verification.ErrConflict
	}
	return nil
}

// EnqueueVerificationJob saves the job, revision binding and pending run together.
func (s *Repository) EnqueueVerificationJob(ctx context.Context, request verification.JobRequest) (verification.Job, error) {
	if err := require(ctx, request.Binding.Scope); err != nil {
		return verification.Job{}, err
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return verification.Job{}, err
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return verification.Job{}, verification.ErrCommitUnavailable
	}
	var result verification.Job
	err = driver.TransactRun(ctx, req.Binding.RunID, func(txctx context.Context, tx RunTransaction) error {
		result = verification.Job{}
		repository := s.inTransaction(tx)
		existing, readErr := repository.GetVerificationJob(txctx, req.ID)
		if readErr == nil {
			if existing.RequestDigest != digest {
				return verification.ErrJobConflict
			}
			result = existing
			return nil
		}
		if !errors.Is(readErr, verification.ErrJobNotFound) {
			return readErr
		}
		current, readErr := tx.GetRun(txctx, req.Binding.RunID)
		if readErr != nil {
			return readErr
		}
		if checkErr := validateCapturedRun(current, req.ExpectedRunRevision, req.ContractID, req.Binding); checkErr != nil {
			return checkErr
		}
		switch current.VerificationStatus.Normalized() {
		case verification.StatusUnchecked, verification.StatusFailed, verification.StatusInconclusive, verification.StatusStale:
		default:
			return verification.ErrJobConflict
		}
		contract, readErr := repository.GetContract(txctx, req.ContractID)
		if readErr != nil {
			return readErr
		}
		if contract.Digest != req.Binding.ContractDigest {
			return verification.ErrConflict
		}
		now := s.clock.Now().UTC()
		if now.IsZero() || !req.Deadline.After(now) {
			return verification.ErrJobExpired
		}
		total := int64(0)
		for _, ref := range append([]evidence.Ref{req.Binding.Candidate}, req.Binding.Evidence...) {
			total += ref.Bytes
			if total > 8<<20 {
				return verification.ErrInvalid
			}
			if req.Deadline.After(ref.RetainUntil) {
				return verification.ErrJobExpired
			}
			if _, loadErr := repository.LoadEvidence(txctx, ref); loadErr != nil {
				return loadErr
			}
		}
		job, sealErr := verification.SealJob(verification.Job{SchemaVersion: 1, Request: req, RequestDigest: digest, RunRevision: current.Revision + 1, Revision: 1, State: verification.JobQueued, CreatedAt: now, UpdatedAt: now})
		if sealErr != nil {
			return sealErr
		}
		row, recordErr := record(KindJob, req.ID.String(), job.Digest, req.Binding.RunID.String(), req.Binding.Scope, job)
		if recordErr != nil {
			return recordErr
		}
		if insertErr := tx.Insert(txctx, row); insertErr != nil {
			return insertErr
		}
		index, recordErr := record(kindJobBinding, jobBindingKey(req.Binding.RunID, job.RunRevision), digest, req.Binding.RunID.String(), req.Binding.Scope, req.ID)
		if recordErr != nil {
			return recordErr
		}
		if insertErr := tx.Insert(txctx, index); insertErr != nil {
			return insertErr
		}
		if updateErr := tx.SetRunVerification(txctx, current.ID, current.Revision, verification.StatusPending, id.ID{}, now); updateErr != nil {
			return updateErr
		}
		result = job
		return nil
	})
	if err != nil {
		return verification.Job{}, err
	}
	return result, nil
}

func (s *Repository) mutateJob(ctx context.Context, jobID id.ID, change func(context.Context, *Repository, RunTransaction, *verification.Job, time.Time) error) (verification.Job, error) {
	initial, err := s.GetVerificationJob(ctx, jobID)
	if err != nil {
		return verification.Job{}, err
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return verification.Job{}, verification.ErrCommitUnavailable
	}
	var result verification.Job
	err = driver.TransactRun(ctx, initial.Request.Binding.RunID, func(txctx context.Context, tx RunTransaction) error {
		result = verification.Job{}
		repository := s.inTransaction(tx)
		job, row, readErr := repository.jobRecord(txctx, jobID)
		if readErr != nil {
			return readErr
		}
		now := s.clock.Now().UTC()
		if now.IsZero() || now.Before(job.UpdatedAt) {
			return verification.ErrInvalid
		}
		if changeErr := change(txctx, repository, tx, &job, now); changeErr != nil {
			return changeErr
		}
		// Idempotent operations leave the entire persisted snapshot unchanged.
		unchanged, encodeErr := json.Marshal(job)
		if encodeErr != nil {
			return encodeErr
		}
		if string(unchanged) == row.Body {
			result = job
			return nil
		}
		if job.Revision == math.MaxInt64 {
			return verification.ErrJobConflict
		}
		job.Revision++
		job.UpdatedAt = now
		sealed, sealErr := verification.SealJob(job)
		if sealErr != nil {
			return sealErr
		}
		replacement, recordErr := record(KindJob, jobID.String(), sealed.Digest, job.Request.Binding.RunID.String(), job.Request.Binding.Scope, sealed)
		if recordErr != nil {
			return recordErr
		}
		if replaceErr := tx.ReplaceJob(txctx, replacement, row.Body); replaceErr != nil {
			return replaceErr
		}
		result = sealed
		return nil
	})
	if err != nil {
		return verification.Job{}, err
	}
	return result, nil
}

func checkJobRun(ctx context.Context, tx RunTransaction, job verification.Job) error {
	current, err := tx.GetRun(ctx, job.Request.Binding.RunID)
	if err != nil {
		return err
	}
	if current.Revision != job.RunRevision || current.VerificationStatus != verification.StatusPending {
		return verification.ErrJobConflict
	}
	return validateCapturedRun(current, job.RunRevision, job.Request.ContractID, job.Request.Binding)
}

func leaseExpiry(now, deadline time.Time, duration time.Duration) (time.Time, error) {
	if duration <= 0 || duration > verification.MaxJobLease {
		return time.Time{}, verification.ErrInvalid
	}
	expiry := now.Add(duration)
	if expiry.After(deadline) {
		expiry = deadline
	}
	if !expiry.After(now) {
		return time.Time{}, verification.ErrJobExpired
	}
	return expiry, nil
}

// ClaimVerificationJob acquires a new generation or recovers the same claim ack.
func (s *Repository) ClaimVerificationJob(ctx context.Context, claim verification.JobClaim) (verification.Job, error) {
	shape := verification.JobLease{JobID: claim.JobID, ClaimID: claim.ClaimID, Owner: claim.Owner, Generation: 1, ExpiresAt: time.Unix(1, 0)}
	if shape.Validate() != nil || claim.Duration <= 0 || claim.Duration > verification.MaxJobLease {
		return verification.Job{}, verification.ErrInvalid
	}
	return s.mutateJob(ctx, claim.JobID, func(txctx context.Context, _ *Repository, tx RunTransaction, job *verification.Job, now time.Time) error {
		if err := checkJobRun(txctx, tx, *job); err != nil {
			return err
		}
		if job.State != verification.JobQueued && job.State != verification.JobRunning && job.State != verification.JobReady {
			return verification.ErrJobConflict
		}
		if job.Lease != nil {
			if job.Lease.ClaimID == claim.ClaimID {
				if job.Lease.Owner != claim.Owner {
					return verification.ErrJobConflict
				}
				return job.CheckLease(*job.Lease, now)
			}
			if job.Lease.ExpiresAt.After(now) {
				return verification.ErrJobConflict
			}
		}
		if slices.Contains(job.ClaimIDs, claim.ClaimID) {
			return verification.ErrJobConflict
		}
		if job.Attempts >= job.Request.MaxAttempts || !job.Request.Deadline.After(now) {
			return verification.ErrJobExpired
		}
		expiry, err := leaseExpiry(now, job.Request.Deadline, claim.Duration)
		if err != nil {
			return err
		}
		job.Attempts++
		job.ClaimIDs = append(job.ClaimIDs, claim.ClaimID)
		job.Generation++
		if job.State != verification.JobReady {
			job.State = verification.JobRunning
		}
		job.Lease = &verification.JobLease{JobID: claim.JobID, ClaimID: claim.ClaimID, Owner: claim.Owner, Generation: job.Generation, ExpiresAt: expiry}
		return nil
	})
}

// RenewVerificationJob extends a live lease without changing its generation.
func (s *Repository) RenewVerificationJob(ctx context.Context, lease verification.JobLease, duration time.Duration) (verification.Job, error) {
	if lease.Validate() != nil {
		return verification.Job{}, verification.ErrInvalid
	}
	return s.mutateJob(ctx, lease.JobID, func(txctx context.Context, _ *Repository, tx RunTransaction, job *verification.Job, now time.Time) error {
		if err := job.CheckLease(lease, now); err != nil {
			return err
		}
		if err := checkJobRun(txctx, tx, *job); err != nil {
			return err
		}
		expiry, err := leaseExpiry(now, job.Request.Deadline, duration)
		if err != nil {
			return err
		}
		if expiry.After(job.Lease.ExpiresAt) {
			job.Lease.ExpiresAt = expiry
		}
		return nil
	})
}
