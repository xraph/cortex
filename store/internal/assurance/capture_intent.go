package assurance

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

const (
	kindCaptureIntent = "capture_intent"
	kindIntentRun     = "capture_intent_run"
	kindIntentJob     = "capture_intent_job"
	kindIntentResult  = "capture_intent_result"
)

var _ verification.CaptureIntentStore = (*Repository)(nil)

// GetCaptureIntent returns immutable, exact-scope host policy.
func (s *Repository) GetCaptureIntent(ctx context.Context, intentID id.ID) (verification.CaptureIntent, error) {
	if intentID.IsNil() || intentID.Prefix() != verification.PrefixCaptureIntent {
		return verification.CaptureIntent{}, verification.ErrInvalid
	}
	row, err := s.get(ctx, Query{Kind: kindCaptureIntent, ID: intentID.String()})
	if errors.Is(err, ErrAbsent) {
		return verification.CaptureIntent{}, verification.ErrNotFound
	}
	if err != nil {
		return verification.CaptureIntent{}, err
	}
	var intent verification.CaptureIntent
	if row.Deleted || len(row.Body) > 1<<20 || json.Unmarshal([]byte(row.Body), &intent) != nil || intent.Validate() != nil {
		return verification.CaptureIntent{}, verification.ErrInvalid
	}
	if row.Kind != kindCaptureIntent || row.ID != intent.ID.String() || row.Digest != intent.Digest || row.RunID != intent.Template.Binding.RunID.String() || row.ScopeCanon != intent.Template.Binding.Scope.Canonical() {
		return verification.CaptureIntent{}, verification.ErrInvalid
	}
	return intent, nil
}

func (s *Repository) indexedCaptureIntent(ctx context.Context, kind, key string) (verification.CaptureIntent, error) {
	row, err := s.get(ctx, Query{Kind: kind, ID: key})
	if errors.Is(err, ErrAbsent) {
		return verification.CaptureIntent{}, verification.ErrNotFound
	}
	if err != nil {
		return verification.CaptureIntent{}, err
	}
	var intentID id.ID
	if row.Deleted || len(row.Body) > 256 || json.Unmarshal([]byte(row.Body), &intentID) != nil {
		return verification.CaptureIntent{}, verification.ErrInvalid
	}
	intent, err := s.GetCaptureIntent(ctx, intentID)
	if err != nil {
		return verification.CaptureIntent{}, err
	}
	if row.Kind != kind || row.ID != key || row.Digest != intent.Digest || row.RunID != intent.Template.Binding.RunID.String() || row.ScopeCanon != intent.Template.Binding.Scope.Canonical() {
		return verification.CaptureIntent{}, verification.ErrInvalid
	}
	switch kind {
	case kindIntentRun:
		if key != intent.Template.Binding.RunID.String() {
			return verification.CaptureIntent{}, verification.ErrInvalid
		}
	case kindIntentJob:
		if key != intent.Template.ID.String() {
			return verification.CaptureIntent{}, verification.ErrInvalid
		}
	case kindIntentResult:
		if key != intent.ResultID.String() {
			return verification.CaptureIntent{}, verification.ErrInvalid
		}
	default:
		return verification.CaptureIntent{}, verification.ErrInvalid
	}
	return intent, nil
}

// GetRunCaptureIntent discovers the stable capture identity after a restart.
func (s *Repository) GetRunCaptureIntent(ctx context.Context, runID id.AgentRunID) (verification.CaptureIntent, error) {
	if runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return verification.CaptureIntent{}, verification.ErrInvalid
	}
	return s.indexedCaptureIntent(ctx, kindIntentRun, runID.String())
}

// CreateCaptureIntent registers policy against a pristine saved running run.
// The host must call it before any provider or tool dispatch; zero counters alone
// cannot prove that an external operation has not already happened.
func (s *Repository) CreateCaptureIntent(ctx context.Context, intent verification.CaptureIntent) error {
	if err := require(ctx, intent.Template.Binding.Scope); err != nil {
		return err
	}
	if err := intent.Validate(); err != nil {
		return err
	}
	frozen, err := verification.SealCaptureIntent(intent)
	if err != nil {
		return err
	}
	if frozen.MaxCandidateBytes > MaxEvidenceBytes {
		return evidence.ErrTooLarge
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return verification.ErrCommitUnavailable
	}
	err = driver.TransactRun(ctx, frozen.Template.Binding.RunID, func(txctx context.Context, tx RunTransaction) error {
		repository := s.inTransaction(tx)
		existing, readErr := repository.GetCaptureIntent(txctx, frozen.ID)
		if readErr == nil {
			if existing.Digest != frozen.Digest {
				return verification.ErrConflict
			}
			return nil
		}
		if !errors.Is(readErr, verification.ErrNotFound) {
			return readErr
		}
		if checkErr := repository.checkNewCaptureIntent(txctx, tx, frozen); checkErr != nil {
			return checkErr
		}
		row, recordErr := record(kindCaptureIntent, frozen.ID.String(), frozen.Digest, frozen.Template.Binding.RunID.String(), frozen.Template.Binding.Scope, frozen)
		if recordErr != nil {
			return recordErr
		}
		if insertErr := tx.Insert(txctx, row); insertErr != nil {
			return insertErr
		}
		for _, index := range []struct{ kind, key string }{{kindIntentRun, frozen.Template.Binding.RunID.String()}, {kindIntentJob, frozen.Template.ID.String()}, {kindIntentResult, frozen.ResultID.String()}} {
			row, recordErr = record(index.kind, index.key, frozen.Digest, frozen.Template.Binding.RunID.String(), frozen.Template.Binding.Scope, frozen.ID)
			if recordErr != nil {
				return recordErr
			}
			if insertErr := tx.Insert(txctx, row); insertErr != nil {
				return insertErr
			}
		}
		return nil
	})
	if errors.Is(err, ErrExists) {
		return verification.ErrConflict
	}
	return err
}

func (s *Repository) checkNewCaptureIntent(ctx context.Context, tx RunTransaction, intent verification.CaptureIntent) error {
	req := intent.Template
	current, err := tx.GetRun(ctx, req.Binding.RunID)
	if err != nil {
		return err
	}
	if current.Scope.Canonical() != req.Binding.Scope.Canonical() || current.Revision != req.ExpectedRunRevision || current.State != run.StateRunning || current.Output != "" || current.ResultRef != "" || current.CompletedAt != nil || current.StopReason != "" || current.StepCount != 0 || current.TokensUsed != 0 || current.Usage != (execution.Usage{}) || current.VerificationStatus.Normalized() != verification.StatusUnchecked || current.Provenance.PreparedDigest != req.Binding.Context.PreparedDigest || current.Provenance.AcceptanceRef != req.ContractID.String() {
		return cortex.ErrRunConflict
	}
	if inputErr := s.validateExecutionInputs(ctx, current, req.Binding); inputErr != nil {
		return inputErr
	}
	contract, err := s.GetContract(ctx, req.ContractID)
	if err != nil {
		return err
	}
	if contract.Digest != req.Binding.ContractDigest {
		return verification.ErrConflict
	}
	now := s.clock.Now().UTC()
	if (!current.Budget.Deadline.IsZero() && req.Deadline.After(current.Budget.Deadline)) || now.IsZero() || !req.Deadline.After(now) || req.Binding.Candidate.CreatedAt.After(now) || req.Deadline.After(req.Binding.Candidate.RetainUntil) {
		return verification.ErrJobExpired
	}
	if _, err = s.GetVerificationJob(ctx, req.ID); !errors.Is(err, verification.ErrJobNotFound) {
		if err != nil {
			return err
		}
		return verification.ErrConflict
	}
	if _, err = s.GetCommittedResult(ctx, intent.ResultID); !errors.Is(err, verification.ErrNotFound) {
		if err != nil {
			return err
		}
		return verification.ErrConflict
	}
	candidateRow, candidateErr := s.get(ctx, Query{Kind: KindEvidence, ID: req.Binding.Candidate.ID.String()})
	if candidateErr == nil {
		if candidateRow.Deleted {
			return evidence.ErrDeleted
		}
		return evidence.ErrConflict
	}
	if !errors.Is(candidateErr, ErrAbsent) {
		return candidateErr
	}
	total := int64(0)
	for _, ref := range req.Binding.Evidence {
		if ref.Bytes > MaxEvidenceBytes || req.Deadline.After(ref.RetainUntil) {
			return verification.ErrInvalid
		}
		total += ref.Bytes
		if total > 8<<20 {
			return evidence.ErrTooLarge
		}
		if _, loadErr := s.LoadEvidence(ctx, ref); loadErr != nil {
			return loadErr
		}
	}
	return nil
}

// CaptureFromIntent derives candidate identity from the saved output and
// captures the job atomically. Historical retries never restore deleted bytes.
func (s *Repository) CaptureFromIntent(ctx context.Context, intentID id.ID) (verification.Job, error) {
	initial, err := s.GetCaptureIntent(ctx, intentID)
	if err != nil {
		return verification.Job{}, err
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return verification.Job{}, verification.ErrCommitUnavailable
	}
	var result verification.Job
	err = driver.TransactRun(ctx, initial.Template.Binding.RunID, func(txctx context.Context, tx RunTransaction) error {
		result = verification.Job{}
		repository := s.inTransaction(tx)
		intent, loadErr := repository.GetCaptureIntent(txctx, intentID)
		if loadErr != nil {
			return loadErr
		}
		if intent.Digest != initial.Digest {
			return verification.ErrConflict
		}
		existing, loadErr := repository.GetVerificationJob(txctx, intent.Template.ID)
		if loadErr == nil {
			if checkErr := matchIntentJob(intent, existing); checkErr != nil {
				return checkErr
			}
			result = existing
			return nil
		}
		if !errors.Is(loadErr, verification.ErrJobNotFound) {
			return loadErr
		}
		current, loadErr := tx.GetRun(txctx, intent.Template.Binding.RunID)
		if loadErr != nil {
			return loadErr
		}
		request, loadErr := intent.JobFor(current.Revision, current.Output)
		if loadErr != nil {
			return loadErr
		}
		request, digest, loadErr := request.Freeze()
		if loadErr != nil {
			return loadErr
		}
		result, loadErr = repository.enqueueJob(txctx, tx, request, digest, true)
		return loadErr
	})
	if err != nil {
		return verification.Job{}, err
	}
	return result, nil
}

func matchIntentJob(intent verification.CaptureIntent, job verification.Job) error {
	return matchIntentRequest(intent, job.Request, job.RequestDigest)
}

func matchIntentRequest(intent verification.CaptureIntent, request verification.JobRequest, requestDigest string) error {
	req, _, err := intent.Template.Freeze()
	if err != nil {
		return err
	}
	if request.ExpectedRunRevision <= req.ExpectedRunRevision || request.Binding.Candidate.Bytes > intent.MaxCandidateBytes {
		return verification.ErrConflict
	}
	req.ExpectedRunRevision = request.ExpectedRunRevision
	req.Binding.Candidate.SHA256 = request.Binding.Candidate.SHA256
	req.Binding.Candidate.Bytes = request.Binding.Candidate.Bytes
	_, digest, err := req.Freeze()
	if err != nil {
		return err
	}
	if requestDigest != digest {
		return verification.ErrConflict
	}
	return nil
}

func (s *Repository) validateEnqueueIntent(ctx context.Context, req verification.JobRequest) error {
	intent, err := s.GetRunCaptureIntent(ctx, req.Binding.RunID)
	if errors.Is(err, verification.ErrNotFound) {
		_, reservedErr := s.indexedCaptureIntent(ctx, kindIntentJob, req.ID.String())
		if errors.Is(reservedErr, verification.ErrNotFound) {
			return nil
		}
		if reservedErr != nil {
			return reservedErr
		}
		return verification.ErrConflict
	}
	if err != nil {
		return err
	}
	_, digest, err := req.Freeze()
	if err != nil {
		return err
	}
	return matchIntentRequest(intent, req, digest)
}

func (s *Repository) validateIntentCommit(ctx context.Context, req verification.CommitRequest) error {
	reserved, err := s.indexedCaptureIntent(ctx, kindIntentResult, req.ResultID.String())
	if err != nil && !errors.Is(err, verification.ErrNotFound) {
		return err
	}
	if err == nil && reserved.Template.Binding.RunID != req.Current.RunID {
		return verification.ErrConflict
	}
	intent, err := s.GetRunCaptureIntent(ctx, req.Current.RunID)
	if errors.Is(err, verification.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if req.ResultID != intent.ResultID || req.JobLease == nil || req.JobLease.JobID != intent.Template.ID {
		return verification.ErrConflict
	}
	return nil
}
