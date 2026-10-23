package assurance

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

// SubmissionRunReader fences the run inside an already-held plan transaction.
// Lock order is plan, run, then evidence. The fence lasts through commit.
type SubmissionRunReader interface {
	LockSubmissionRun(context.Context, id.AgentRunID) (*run.Run, error)
}

func (s *Repository) validateAssignmentSubmission(ctx context.Context, a coordination.Assignment, at time.Time) error {
	pins := a.Submission
	if pins == nil || pins.Validate() != nil {
		return coordination.ErrInvalidAssignment
	}
	reader, ok := s.driver.(SubmissionRunReader)
	if !ok {
		return coordination.ErrAssignmentUnavailable
	}
	current, err := reader.LockSubmissionRun(ctx, pins.RunID)
	if err != nil {
		return err
	}
	job, err := s.GetVerificationJob(ctx, pins.JobID)
	if err != nil {
		return err
	}
	binding := job.Request.Binding
	digest, err := binding.Digest()
	if err != nil {
		return err
	}
	if job.RequestDigest != pins.JobRequestDigest || digest != pins.BindingDigest || binding.RunID != pins.RunID || binding.Context.PrincipalRef != a.Handoff.Receiver.Principal || !job.Request.Deadline.After(at) || job.CreatedAt.After(at) || current.ID != pins.RunID || current.Scope.Canonical() != a.Handoff.Scope.Canonical() || current.Revision != pins.RunRevision || current.State != run.StateCompleted || current.StopReason != run.StopFinalAnswer || current.Provenance.ParentRunID != a.Handoff.ParentRunID.String() || current.Provenance.AssignmentDigest != a.Handoff.Digest || current.Provenance.PreparedDigest != binding.Context.PreparedDigest || current.Provenance.AcceptanceRef != job.Request.ContractID.String() {
		return coordination.ErrInvalidAssignment
	}
	p, err := s.GetPlan(ctx, a.Handoff.PlanID)
	if err != nil {
		return err
	}
	contract, err := s.GetContract(ctx, job.Request.ContractID)
	if err != nil {
		return err
	}
	if planning.ValidateTaskBindingForContract(p, a.Handoff.TaskID, contract, binding) != nil {
		return coordination.ErrInvalidAssignment
	}
	refs := append([]evidence.Ref{binding.Candidate}, binding.Evidence...)
	if len(refs) > 64 {
		return evidence.ErrTooLarge
	}
	var bytes int64
	for _, ref := range refs {
		if ref.CreatedAt.After(at) || !ref.RetainUntil.After(at) {
			return coordination.ErrInvalidAssignment
		}
		if ref.Bytes > MaxEvidenceBytes || ref.Bytes > (8<<20)-bytes {
			return evidence.ErrTooLarge
		}
		bytes += ref.Bytes
	}
	if stateErr := s.validateSubmissionRunState(ctx, current, job); stateErr != nil {
		return stateErr
	}
	if inputsErr := s.validateExecutionInputs(ctx, current, binding); inputsErr != nil {
		return inputsErr
	}
	// Check all bounds before reading any supporting bytes. Backend transaction
	// reads fence concurrent tombstones; no network calls run under these locks.
	for _, ref := range refs {
		if _, loadErr := s.LoadEvidence(ctx, ref); loadErr != nil {
			return loadErr
		}
	}
	observed := s.clock.Now()
	if observed.IsZero() || observed.Before(at) || !observed.Before(a.Handoff.Deadline) || !observed.Before(job.Request.Deadline) {
		return coordination.ErrInvalidAssignment
	}
	for _, ref := range refs {
		if !ref.RetainUntil.After(observed) {
			return coordination.ErrInvalidAssignment
		}
	}
	return ctx.Err()
}

func (s *Repository) validateSubmissionRunState(ctx context.Context, current *run.Run, job verification.Job) error {
	switch job.State {
	case verification.JobCommitted:
		receipt, err := s.GetCommittedResult(ctx, job.ResultID)
		if err != nil {
			return err
		}
		digest, err := receipt.Request.Current.Digest()
		if err != nil {
			return err
		}
		expected, err := job.Request.Binding.Digest()
		if err != nil {
			return err
		}
		if current.VerificationStatus != verification.StatusPassed || current.ResultRef != job.ResultID.String() || current.Revision != receipt.RunRevision || receipt.Request.ExpectedRunRevision != job.RunRevision || receipt.Request.ContractID != job.Request.ContractID || digest != expected || receipt.Request.JobLease == nil || receipt.Request.JobLease.JobID != job.Request.ID {
			return cortex.ErrRunConflict
		}
		return nil
	case verification.JobQueued, verification.JobRunning, verification.JobReady:
		if current.Revision != job.RunRevision || current.VerificationStatus != verification.StatusPending {
			return cortex.ErrRunConflict
		}
	case verification.JobFailed, verification.JobInconclusive:
		expected := verification.StatusFailed
		if job.State == verification.JobInconclusive {
			expected = verification.StatusInconclusive
		}
		if job.State == verification.JobInconclusive && current.VerificationStatus == verification.StatusStale {
			expected = verification.StatusStale
		}
		if current.Revision != job.RunRevision+1 || current.VerificationStatus != expected {
			return cortex.ErrRunConflict
		}
	default:
		return coordination.ErrInvalidAssignment
	}
	if current.ResultRef != "" {
		return cortex.ErrRunConflict
	}
	return s.validateCapturedOutput(ctx, current, job.Request.Binding)
}
