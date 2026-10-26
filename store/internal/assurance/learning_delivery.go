package assurance

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/learning"
)

func (s *Repository) updateLessonDelivery(ctx context.Context, identifier id.ID, revision int64, update func(*learning.Publication) error) (learning.Publication, error) {
	initial, err := s.GetLessonPublication(ctx, identifier, revision)
	if err != nil {
		return learning.Publication{}, err
	}
	var result learning.Publication
	err = s.lessonTransaction(ctx, initial.SourceID, func(tc context.Context, _ RunTransaction, repository *Repository) error {
		current, readErr := repository.GetLessonPublication(tc, identifier, revision)
		if readErr != nil {
			return readErr
		}
		if current.Generation == math.MaxInt64 {
			return learning.ErrConflict
		}
		if updateErr := update(&current); updateErr != nil {
			return updateErr
		}
		current.Generation++
		body, encodeErr := json.Marshal(current)
		if encodeErr != nil {
			return encodeErr
		}
		group := lessonRevisionKey(identifier, revision)
		row, recordErr := record(kindLessonDelivery, fmt.Sprintf("%s/%020d", group, current.Generation), evidence.Digest(body), group, current.Payload.Scope, current)
		if recordErr != nil {
			return recordErr
		}
		if writeErr := repository.driver.Insert(tc, row); writeErr != nil {
			return writeErr
		}
		result = current
		return tc.Err()
	})
	if err != nil {
		return learning.Publication{}, err
	}
	return result, ctx.Err()
}

// ClaimLessonPublication fences one bounded delivery attempt across processes.
func (s *Repository) ClaimLessonPublication(ctx context.Context, identifier id.ID, revision int64, owner string) (learning.Publication, error) {
	if strings.TrimSpace(owner) == "" || len(owner) > 512 {
		return learning.Publication{}, learning.ErrInvalid
	}
	return s.updateLessonDelivery(ctx, identifier, revision, func(p *learning.Publication) error {
		now := s.clock.Now().UTC()
		if p.Receipt != nil || p.Attempts >= 5 || p.LeaseUntil.After(now) || p.NextAttempt.After(now) {
			return learning.ErrConflict
		}
		p.Attempts++
		p.Owner = owner
		p.LeaseUntil = now.Add(30 * time.Second)
		p.NextAttempt = time.Time{}
		return nil
	})
}

// CompleteLessonPublication records a delivery receipt or bounded retry status.
// A lost response leaves the original aggregate identity unchanged.
func (s *Repository) CompleteLessonPublication(ctx context.Context, claim learning.Publication, receipt *learning.Receipt, code string) error {
	if receipt == nil && code != "unavailable" && code != "denied" && code != "ineligible" && code != "conflict" {
		return learning.ErrInvalid
	}
	if receipt != nil && (code != "" || receipt.LessonID != claim.LessonID || receipt.PayloadDigest != claim.PayloadDigest || receipt.AggregateID != claim.AggregateID || receipt.Version < 1 || receipt.AcknowledgedAt.IsZero()) {
		return learning.ErrInvalid
	}
	_, err := s.updateLessonDelivery(ctx, claim.LessonID, claim.Revision, func(p *learning.Publication) error {
		if p.PayloadDigest != claim.PayloadDigest || p.AggregateID != claim.AggregateID || p.SourceID != claim.SourceID || p.Operation != claim.Operation || p.Generation != claim.Generation || p.Owner != claim.Owner || !p.LeaseUntil.After(s.clock.Now().UTC()) || p.Receipt != nil {
			return learning.ErrConflict
		}
		p.Receipt = receipt
		p.ErrorCode = code
		p.Owner = ""
		p.LeaseUntil = time.Time{}
		if receipt == nil {
			p.NextAttempt = s.clock.Now().UTC().Add(time.Duration(p.Attempts) * time.Second)
		}
		return nil
	})
	return err
}

// RetryLessonPublication starts another bounded cycle at the expected generation.
// The host must authorize this operator action before calling trusted storage.
func (s *Repository) RetryLessonPublication(ctx context.Context, identifier id.ID, revision, generation int64) error {
	_, err := s.updateLessonDelivery(ctx, identifier, revision, func(p *learning.Publication) error {
		if generation < 1 || p.Generation != generation || p.LeaseUntil.After(s.clock.Now().UTC()) || p.Receipt != nil {
			return learning.ErrConflict
		}
		p.Attempts = 0
		p.NextAttempt = time.Time{}
		p.ErrorCode = ""
		return nil
	})
	return err
}
