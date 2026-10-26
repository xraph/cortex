package assurance

import (
	"context"

	"github.com/xraph/cortex/verification"
)

// loadReportObservations verifies immutable output evidence before storing or
// consuming a report. It contributes to the caller's total material byte bound.
func (s *Repository) loadReportObservations(ctx context.Context, r verification.Report, limit int) (int, error) {
	refs, err := verification.ReportObservationRefs(r)
	if err != nil {
		return 0, err
	}
	total := 0
	for _, ref := range refs {
		if ref.Bytes > int64(limit-total) {
			return 0, verification.ErrInvalid
		}
		if ref.CreatedAt.After(s.clock.Now()) || !s.clock.Now().Before(ref.RetainUntil) {
			return 0, verification.ErrInvalid
		}
		artifact, readErr := s.LoadEvidence(ctx, ref)
		if readErr != nil {
			return 0, readErr
		}
		total += len(artifact.Data)
	}
	for _, ref := range refs {
		if !s.clock.Now().Before(ref.RetainUntil) {
			return 0, verification.ErrInvalid
		}
	}
	return total, ctx.Err()
}
