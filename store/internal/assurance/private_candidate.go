package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

const kindPrivateCandidate = "private_candidate"

type privateCandidate struct {
	SchemaVersion     int             `json:"schema_version"`
	IntentID          id.ID           `json:"intent_id"`
	RequestDigest     string          `json:"request_digest"`
	CompletedRevision int64           `json:"completed_revision"`
	Candidate         evidence.Ref    `json:"candidate"`
	FinishReason      string          `json:"finish_reason"`
	Usage             execution.Usage `json:"usage"`
}

var _ run.CandidateStore = (*Repository)(nil)

func (s *Repository) loadPrivateCandidate(ctx context.Context, runID id.AgentRunID) (privateCandidate, bool, error) {
	row, err := s.get(ctx, Query{Kind: kindPrivateCandidate, ID: runID.String()})
	if errors.Is(err, ErrAbsent) {
		return privateCandidate{}, false, nil
	}
	if err != nil {
		return privateCandidate{}, false, err
	}
	var c privateCandidate
	if row.Deleted || len(row.Body) > 1<<20 || row.Kind != kindPrivateCandidate || row.ID != runID.String() || row.RunID != runID.String() || evidence.Digest([]byte(row.Body)) != row.Digest || json.Unmarshal([]byte(row.Body), &c) != nil || c.Candidate.Validate() != nil || c.Candidate.Scope.Canonical() != row.ScopeCanon || !evidence.ValidDigest(c.RequestDigest) {
		return privateCandidate{}, false, verification.ErrInvalid
	}
	shape := run.CandidateCapture{SchemaVersion: c.SchemaVersion, IntentID: c.IntentID, ExpectedRunRevision: c.CompletedRevision - 1, FinishReason: c.FinishReason, Usage: c.Usage}
	if _, _, err = shape.Freeze(); err != nil {
		return privateCandidate{}, false, err
	}
	return c, true, nil
}

// CaptureCandidate finishes generation without putting its text in the run row.
// Evidence, completion metadata, job and pending status share one transaction.
func (s *Repository) CaptureCandidate(ctx context.Context, request run.CandidateCapture) (verification.Job, error) {
	if len(request.Output) > MaxEvidenceBytes {
		return verification.Job{}, evidence.ErrTooLarge
	}
	req, digest, err := request.Freeze()
	if err != nil {
		return verification.Job{}, err
	}
	initial, err := s.GetCaptureIntent(ctx, req.IntentID)
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
		intent, readErr := repository.GetCaptureIntent(txctx, req.IntentID)
		if readErr != nil {
			return readErr
		}
		if intent.Digest != initial.Digest {
			return verification.ErrConflict
		}
		previous, exists, readErr := repository.loadPrivateCandidate(txctx, intent.Template.Binding.RunID)
		if readErr != nil {
			return readErr
		}
		if exists {
			if previous.IntentID != req.IntentID || previous.RequestDigest != digest {
				return verification.ErrConflict
			}
			job, jobErr := repository.GetVerificationJob(txctx, intent.Template.ID)
			if jobErr != nil {
				return jobErr
			}
			if matchErr := matchIntentJob(intent, job); matchErr != nil {
				return matchErr
			}
			if !reflect.DeepEqual(previous.Candidate, job.Request.Binding.Candidate) || previous.CompletedRevision != job.Request.ExpectedRunRevision {
				return verification.ErrConflict
			}
			result = job
			return nil
		}
		result, readErr = repository.captureNewCandidate(txctx, tx, intent, req, digest)
		return readErr
	})
	if err != nil {
		return verification.Job{}, err
	}
	return result, nil
}

func (s *Repository) captureNewCandidate(ctx context.Context, tx RunTransaction, intent verification.CaptureIntent, capture run.CandidateCapture, digest string) (verification.Job, error) {
	current, err := tx.GetRun(ctx, intent.Template.Binding.RunID)
	if err != nil {
		return verification.Job{}, err
	}
	if current.Revision != capture.ExpectedRunRevision || current.Revision < intent.Template.ExpectedRunRevision || current.Scope.Canonical() != intent.Template.Binding.Scope.Canonical() || current.State != run.StateRunning || current.Output != "" || current.ResultRef != "" || current.CompletedAt != nil || current.StopReason != "" || current.VerificationStatus.Normalized() != verification.StatusUnchecked || current.Provenance.PreparedDigest != intent.Template.Binding.Context.PreparedDigest || current.Provenance.AcceptanceRef != intent.Template.ContractID.String() || !validCaptureUsage(current, capture.Usage) {
		return verification.Job{}, cortex.ErrRunConflict
	}
	if inputErr := s.validateExecutionInputs(ctx, current, intent.Template.Binding); inputErr != nil {
		return verification.Job{}, inputErr
	}
	jobRequest, err := intent.JobFor(current.Revision+1, capture.Output)
	if err != nil {
		return verification.Job{}, err
	}
	jobRequest, jobDigest, err := jobRequest.Freeze()
	if err != nil {
		return verification.Job{}, err
	}
	if captureErr := s.captureCandidate(ctx, jobRequest.Binding.Candidate, capture.Output); captureErr != nil {
		return verification.Job{}, captureErr
	}
	metadata := privateCandidate{SchemaVersion: 1, IntentID: intent.ID, RequestDigest: digest, CompletedRevision: current.Revision + 1, Candidate: jobRequest.Binding.Candidate, FinishReason: capture.FinishReason, Usage: capture.Usage}
	data, err := json.Marshal(metadata)
	if err != nil {
		return verification.Job{}, err
	}
	row, err := record(kindPrivateCandidate, current.ID.String(), evidence.Digest(data), current.ID.String(), current.Scope, metadata)
	if err != nil {
		return verification.Job{}, err
	}
	if insertErr := tx.Insert(ctx, row); insertErr != nil {
		return verification.Job{}, insertErr
	}
	now := s.clock.Now().UTC()
	current.State, current.StopReason, current.FinishReason = run.StateCompleted, run.StopFinalAnswer, capture.FinishReason
	current.Usage, current.StepCount, current.TokensUsed = capture.Usage, capture.Usage.Steps, capture.Usage.Tokens
	current.CompletedAt = &now
	if updateErr := tx.SetRunCandidate(ctx, current, now); updateErr != nil {
		return verification.Job{}, updateErr
	}
	return s.enqueueJob(ctx, tx, jobRequest, jobDigest, false)
}

func validCaptureUsage(current *run.Run, usage execution.Usage) bool {
	old, budget := current.Usage, current.Budget
	return usage.Steps >= old.Steps && usage.Steps >= current.StepCount && usage.Tokens >= old.Tokens && usage.Tokens >= current.TokensUsed && usage.ReportedTokens >= old.ReportedTokens && usage.ToolCalls >= old.ToolCalls && (!old.Estimated || usage.Estimated) && (budget.MaxSteps == 0 || usage.Steps <= budget.MaxSteps) && (budget.MaxTokens == 0 || usage.Tokens <= budget.MaxTokens) && (budget.MaxToolCalls == 0 || usage.ToolCalls <= budget.MaxToolCalls)
}

func (s *Repository) validateCapturedOutput(ctx context.Context, r *run.Run, binding verification.Binding) error {
	candidate, exists, err := s.loadPrivateCandidate(ctx, r.ID)
	if err != nil {
		return err
	}
	if exists {
		if r.Output != "" || r.CompletedAt == nil || r.Revision < candidate.CompletedRevision || r.FinishReason != candidate.FinishReason || r.Usage != candidate.Usage || r.StepCount != candidate.Usage.Steps || r.TokensUsed != candidate.Usage.Tokens || !reflect.DeepEqual(candidate.Candidate, binding.Candidate) {
			return verification.ErrConflict
		}
		return nil
	}
	if int64(len(r.Output)) != binding.Candidate.Bytes || evidence.Digest([]byte(r.Output)) != binding.Candidate.SHA256 {
		return verification.ErrConflict
	}
	return nil
}

func (s *Repository) publishCandidateResult(ctx context.Context, tx RunTransaction, r *run.Run, resultID id.ID, output string, at time.Time) error {
	_, private, err := s.loadPrivateCandidate(ctx, r.ID)
	if err != nil {
		return err
	}
	if !private {
		return tx.SetRunVerification(ctx, r.ID, r.Revision, verification.StatusPassed, resultID, at)
	}
	r.Output, r.ResultRef, r.VerificationStatus = output, resultID.String(), verification.StatusPassed
	return tx.SetRunCandidate(ctx, r, at)
}
