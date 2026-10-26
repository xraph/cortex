package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/learning"
)

const kindLessonTombstone = "lesson_tombstone"

var retentionCode = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// LessonRetentionDriver redacts every local payload revision and delivery copy.
// Implementations must run inside the source run transaction and exact scope.
type LessonRetentionDriver interface {
	EraseLessonPayloads(context.Context, id.ID) error
	FenceLessonRevision(context.Context, string, string) error
}

// DeleteLesson atomically erases lesson and outbox bodies and reserves its ID.
// A repeated matching request returns its original content-free audit record.
func (s *Repository) DeleteLesson(ctx context.Context, request learning.DeletionRequest) (learning.Tombstone, error) {
	if request.ID.IsNil() || request.ID.Prefix() != learning.PrefixLesson || request.ExpectedRevision < 1 || strings.TrimSpace(request.Actor) == "" || len(request.Actor) > 512 || !retentionCode.MatchString(request.Reason) {
		return learning.Tombstone{}, learning.ErrInvalid
	}
	previous, err := s.GetLessonTombstone(ctx, request.ID)
	if err == nil {
		if previous.Revision != request.ExpectedRevision || previous.Actor != request.Actor || previous.Reason != request.Reason {
			return learning.Tombstone{}, learning.ErrConflict
		}
		return previous, nil
	}
	if !errors.Is(err, learning.ErrNotFound) {
		return learning.Tombstone{}, err
	}
	lesson, err := s.GetLesson(ctx, request.ID)
	if err != nil || lesson.Result == nil {
		return learning.Tombstone{}, errors.Join(err, learning.ErrIneligible)
	}
	initialDigest, digestErr := learning.Digest(lesson)
	if digestErr != nil {
		return learning.Tombstone{}, digestErr
	}
	var result learning.Tombstone
	err = s.lessonTransaction(ctx, lesson.Result.RunID, func(tc context.Context, _ RunTransaction, repository *Repository) error {
		driver, ok := repository.driver.(LessonRetentionDriver)
		if !ok {
			return learning.ErrUnavailable
		}
		// Corrections use their new source run. Lock the shared lesson revision before
		// reading its latest revision so a waited PostgreSQL statement cannot erase
		// an older snapshot while a correction inserts a newer payload behind it.
		if fenceErr := driver.FenceLessonRevision(tc, lessonRevisionKey(lesson.ID, lesson.Revision), initialDigest); fenceErr != nil {
			return fenceErr
		}
		if _, tombstoneErr := repository.GetLessonTombstone(tc, request.ID); tombstoneErr == nil {
			return learning.ErrIneligible
		} else if !errors.Is(tombstoneErr, learning.ErrNotFound) {
			return tombstoneErr
		}
		current, readErr := repository.GetLesson(tc, request.ID)
		if readErr != nil {
			return readErr
		}
		if current.Revision != request.ExpectedRevision {
			return learning.ErrConflict
		}
		digest, digestErr := learning.Digest(current)
		if digestErr != nil {
			return digestErr
		}
		result = learning.Tombstone{ID: request.ID, Scope: current.Scope, Revision: current.Revision, Digest: digest, AggregateID: "learning/" + request.ID.String(), Actor: request.Actor, Reason: request.Reason, DeletedAt: s.clock.Now().UTC()}
		row, recordErr := record(kindLessonTombstone, request.ID.String(), digest, current.Result.RunID.String(), current.Scope, result)
		if recordErr != nil {
			return recordErr
		}
		if writeErr := repository.driver.Insert(tc, row); writeErr != nil {
			return writeErr
		}
		return driver.EraseLessonPayloads(tc, request.ID)
	})
	if err != nil {
		return learning.Tombstone{}, err
	}
	return result, ctx.Err()
}

// GetLessonTombstone reads the content-free deletion audit under exact scope.
func (s *Repository) GetLessonTombstone(ctx context.Context, identifier id.ID) (learning.Tombstone, error) {
	if identifier.IsNil() || identifier.Prefix() != learning.PrefixLesson {
		return learning.Tombstone{}, learning.ErrInvalid
	}
	row, err := s.get(ctx, Query{Kind: kindLessonTombstone, ID: identifier.String()})
	if errors.Is(err, ErrAbsent) {
		return learning.Tombstone{}, learning.ErrNotFound
	}
	if err != nil {
		return learning.Tombstone{}, err
	}
	var value learning.Tombstone
	if json.Unmarshal([]byte(row.Body), &value) != nil || value.ID != identifier || value.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() || value.Digest != row.Digest || value.DeletedAt.IsZero() {
		return learning.Tombstone{}, learning.ErrConflict
	}
	return value, ctx.Err()
}

// LessonPayloadKinds is the complete list of records containing lesson content.
func LessonPayloadKinds() []string {
	return []string{kindLesson, kindLessonPublication, kindLessonDelivery}
}

// ListLessonTombstones supplies durable restartable destination cleanup work.
func (s *Repository) ListLessonTombstones(ctx context.Context, after string, limit int) (learning.TombstonePage, error) {
	if limit < 1 || limit > 100 || len(after) > 128 {
		return learning.TombstonePage{}, learning.ErrInvalid
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindLessonTombstone, After: after, Limit: limit})
	if err != nil {
		return learning.TombstonePage{}, err
	}
	page := learning.TombstonePage{Items: []learning.Tombstone{}}
	for _, row := range rows {
		identifier, parseErr := id.Parse(row.ID)
		if parseErr != nil {
			return learning.TombstonePage{}, learning.ErrConflict
		}
		tombstone, readErr := s.GetLessonTombstone(ctx, identifier)
		if readErr != nil {
			return learning.TombstonePage{}, readErr
		}
		page.Items = append(page.Items, tombstone)
		page.After = row.ID
	}
	if len(rows) < limit {
		page.After = ""
	}
	return page, ctx.Err()
}
