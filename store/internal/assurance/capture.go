package assurance

import (
	"context"
	"errors"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/verification"
)

var _ verification.CaptureStore = (*Repository)(nil)

// CaptureVerificationJob captures candidate bytes from the stored run in the
// same transaction as enqueue. The host authorizes capture before calling it.
func (s *Repository) CaptureVerificationJob(ctx context.Context, request verification.JobRequest) (verification.Job, error) {
	return s.enqueueVerificationJob(ctx, request, true)
}

// captureCandidate runs inside the run transaction after validating the saved
// output against the requested reference. Check existing identity first so an
// idempotent upload does not abort a PostgreSQL transaction on a unique key.
func (s *Repository) captureCandidate(ctx context.Context, ref evidence.Ref, output string) error {
	return s.captureEvidence(ctx, ref, []byte(output))
}

// captureEvidence shares candidate and transcript identity checks inside a run transaction.
func (s *Repository) captureEvidence(ctx context.Context, ref evidence.Ref, data []byte) error {
	if len(data) > MaxEvidenceBytes {
		return evidence.ErrTooLarge
	}
	_, err := s.LoadEvidence(ctx, ref)
	if err == nil {
		return nil
	}
	if !errors.Is(err, evidence.ErrNotFound) {
		return err
	}
	return s.SaveEvidence(ctx, evidence.Artifact{Ref: ref, Data: data})
}
