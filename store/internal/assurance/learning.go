package assurance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/learning"
	"github.com/xraph/cortex/verification"
)

const (
	kindLesson            = "lesson"
	kindLessonPublication = "lesson_publication"
	kindLessonDelivery    = "lesson_delivery"
)

var _ learning.Repository = (*Repository)(nil)

func lessonRevisionKey(identifier id.ID, revision int64) string {
	return fmt.Sprintf("%s/%020d", identifier.String(), revision)
}

// AcceptLesson saves the exact verified output and its publication intent in one
// source-run transaction. Remote services and host authority run outside it.
func (s *Repository) AcceptLesson(ctx context.Context, request learning.AcceptRequest) (learning.Publication, error) {
	body, err := json.Marshal(request)
	if err != nil || len(body) > 32768 {
		return learning.Publication{}, learning.ErrInvalid
	}
	var req learning.AcceptRequest
	if json.Unmarshal(body, &req) != nil || req.ID.IsNil() || req.ID.Prefix() != learning.PrefixLesson || req.Result.Validate() != nil || strings.TrimSpace(req.Actor) == "" || len(req.Actor) > 512 || learning.ValidateContent(req.Domain, "pending", req.Applicability, req.Contraindications) != nil {
		return learning.Publication{}, learning.ErrInvalid
	}
	digest := evidence.Digest(body)
	var result learning.Publication
	err = s.lessonTransaction(ctx, req.Result.RunID, func(tc context.Context, tx RunTransaction, repository *Repository) error {
		result = learning.Publication{}
		if _, deletedErr := repository.GetLessonTombstone(tc, req.ID); deletedErr == nil {
			return learning.ErrIneligible
		} else if !errors.Is(deletedErr, learning.ErrNotFound) {
			return deletedErr
		}
		current, readErr := tx.GetRun(tc, req.Result.RunID)
		if readErr != nil {
			return readErr
		}
		if current == nil || learning.ValidatePurpose(current.Provenance.Purpose, current.State, current.VerificationStatus) != nil || learning.ValidateProvenance(current.Provenance) != nil {
			return learning.ErrIneligible
		}
		snapshot, _, readErr := repository.resultSnapshotForRun(tc, req.Result, current, 8<<20)
		if readErr != nil {
			return readErr
		}
		existing, readErr := repository.GetLessonPublication(tc, req.ID, 1)
		if readErr == nil {
			if existing.AcceptanceDigest != digest {
				return learning.ErrConflict
			}
			if _, resolveErr := repository.lessonForSnapshot(tc, existing.Payload, snapshot); resolveErr != nil {
				return resolveErr
			}
			result = existing
			return nil
		}
		if !errors.Is(readErr, learning.ErrNotFound) {
			return readErr
		}
		now := s.clock.Now().UTC()
		lesson := learning.Lesson{ID: req.ID, Scope: cortex.ScopeFromContext(tc), Domain: req.Domain, HarnessSchema: 1, Statement: snapshot.Output, Applicability: req.Applicability, Contraindications: req.Contraindications, EvidenceClass: learning.EvidenceValidated, Revision: 1, CreatedAt: now, ExpiresAt: req.ExpiresAt, Result: &req.Result, Corrects: req.Corrects}
		for _, report := range snapshot.Reports {
			lesson.SourceReports = append(lesson.SourceReports, learning.ReportRef{ID: report.ID, Digest: report.Digest})
		}
		if validationErr := learning.ValidateSnapshot(lesson, snapshot, now); validationErr != nil {
			return validationErr
		}
		payloadDigest, digestErr := learning.Digest(lesson)
		if digestErr != nil {
			return digestErr
		}
		result = learning.Publication{LessonID: lesson.ID, Revision: 1, SourceID: req.Result.RunID, Operation: "publish", Payload: lesson, PayloadDigest: payloadDigest, AggregateID: "learning/" + lesson.ID.String(), AcceptanceDigest: digest, Actor: req.Actor}
		if req.Corrects != nil {
			if correctionErr := repository.correctLesson(tc, *req.Corrects, result, req.Actor); correctionErr != nil {
				return correctionErr
			}
		}
		return repository.insertLessonPublication(tc, result)
	})
	if err != nil {
		return learning.Publication{}, err
	}
	return result, ctx.Err()
}

func (s *Repository) lessonTransaction(ctx context.Context, runID id.AgentRunID, action func(context.Context, RunTransaction, *Repository) error) error {
	if _, err := cortex.RequireScope(ctx); err != nil {
		return err
	}
	if runID.IsNil() || runID.Prefix() != id.PrefixAgentRun {
		return learning.ErrInvalid
	}
	driver, ok := s.driver.(RunDriver)
	if !ok {
		return learning.ErrUnavailable
	}
	err := driver.TransactRun(ctx, runID, func(tc context.Context, tx RunTransaction) error { return action(tc, tx, s.inTransaction(tx)) })
	if errors.Is(err, ErrExists) {
		return learning.ErrConflict
	}
	return err
}

func (s *Repository) insertLessonPublication(ctx context.Context, publication learning.Publication) error {
	key := lessonRevisionKey(publication.LessonID, publication.Revision)
	for _, value := range []struct {
		kind, group string
		data        any
	}{{kindLesson, publication.LessonID.String(), publication.Payload}, {kindLessonPublication, publication.SourceID.String(), publication}} {
		row, err := record(value.kind, key, publication.PayloadDigest, value.group, publication.Payload.Scope, value.data)
		if err != nil {
			return err
		}
		if insertErr := s.driver.Insert(ctx, row); insertErr != nil {
			return insertErr
		}
	}
	return ctx.Err()
}

// GetLesson returns the latest stored revision, including withdrawal metadata.
func (s *Repository) GetLesson(ctx context.Context, identifier id.ID) (learning.Lesson, error) {
	if identifier.IsNil() || identifier.Prefix() != learning.PrefixLesson {
		return learning.Lesson{}, learning.ErrInvalid
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindLesson, RunID: identifier.String(), Reverse: true, Limit: 1})
	if err != nil {
		return learning.Lesson{}, err
	}
	if len(rows) != 1 {
		return learning.Lesson{}, learning.ErrNotFound
	}
	row := rows[0]
	if row.Deleted {
		return learning.Lesson{}, learning.ErrIneligible
	}
	var lesson learning.Lesson
	if json.Unmarshal([]byte(row.Body), &lesson) != nil || row.Deleted || lesson.ID != identifier || lesson.Revision < 1 || row.ID != lessonRevisionKey(identifier, lesson.Revision) || row.ScopeCanon != cortex.ScopeFromContext(ctx).Canonical() || lesson.Scope.Canonical() != row.ScopeCanon {
		return learning.Lesson{}, learning.ErrConflict
	}
	digest, err := learning.Digest(lesson)
	if err != nil || digest != row.Digest {
		return learning.Lesson{}, learning.ErrConflict
	}
	return lesson, ctx.Err()
}

// ResolveLesson rechecks local withdrawal and current saved source evidence.
// The host still must authorize content and fence external source revisions.
func (s *Repository) ResolveLesson(ctx context.Context, ref learning.Ref) (learning.Lesson, error) {
	lesson, err := s.GetLesson(ctx, ref.ID)
	if err != nil {
		return learning.Lesson{}, err
	}
	digest, err := learning.Digest(lesson)
	if err != nil || lesson.Revision != ref.Revision || digest != ref.Digest || lesson.Validate(s.clock.Now().UTC()) != nil {
		return learning.Lesson{}, learning.ErrIneligible
	}
	var resolved learning.Lesson
	var snapshot verification.ResultSnapshot
	err = s.lessonTransaction(ctx, lesson.Result.RunID, func(tc context.Context, tx RunTransaction, repository *Repository) error {
		current, readErr := tx.GetRun(tc, lesson.Result.RunID)
		if readErr != nil {
			return readErr
		}
		if current == nil || learning.ValidatePurpose(current.Provenance.Purpose, current.State, current.VerificationStatus) != nil || learning.ValidateProvenance(current.Provenance) != nil {
			return learning.ErrIneligible
		}
		snapshot, _, readErr = repository.resultSnapshotForRun(tc, *lesson.Result, current, 8<<20)
		if readErr != nil {
			return readErr
		}
		resolved, readErr = repository.lessonForSnapshot(tc, lesson, snapshot)
		return readErr
	})
	if err != nil {
		return learning.Lesson{}, err
	}
	if learning.ValidateSnapshot(resolved, snapshot, s.clock.Now().UTC()) != nil {
		return learning.Lesson{}, learning.ErrIneligible
	}
	return resolved, ctx.Err()
}

func (s *Repository) lessonForSnapshot(ctx context.Context, expected learning.Lesson, snapshot verification.ResultSnapshot) (learning.Lesson, error) {
	current, err := s.GetLesson(ctx, expected.ID)
	if err != nil {
		return learning.Lesson{}, err
	}
	before, err := learning.Digest(expected)
	if err != nil {
		return learning.Lesson{}, err
	}
	after, err := learning.Digest(current)
	if err != nil || before != after || learning.ValidateSnapshot(current, snapshot, s.clock.Now().UTC()) != nil {
		return learning.Lesson{}, learning.ErrIneligible
	}
	return current, ctx.Err()
}

// RevokeLesson withdraws local eligibility before remote delivery. Source expiry
// or loss cannot prevent withdrawal, and retries keep the same outbox identity.
func (s *Repository) RevokeLesson(ctx context.Context, request learning.RevokeRequest) (learning.Publication, error) {
	if strings.TrimSpace(request.Actor) == "" || len(request.Actor) > 512 || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 4096 || request.ExpectedRevision < 1 || request.ExpectedRevision == math.MaxInt64 {
		return learning.Publication{}, learning.ErrInvalid
	}
	lesson, err := s.GetLesson(ctx, request.ID)
	if err != nil || lesson.Result == nil {
		return learning.Publication{}, errors.Join(err, learning.ErrIneligible)
	}
	var result learning.Publication
	err = s.lessonTransaction(ctx, lesson.Result.RunID, func(tc context.Context, _ RunTransaction, repository *Repository) error {
		current, readErr := repository.GetLesson(tc, request.ID)
		if readErr != nil {
			return readErr
		}
		if current.Revision == request.ExpectedRevision+1 && !current.RevokedAt.IsZero() && current.RevocationReason == request.Reason {
			result, readErr = repository.GetLessonPublication(tc, current.ID, current.Revision)
			if readErr == nil && result.Actor != request.Actor {
				return learning.ErrConflict
			}
			return readErr
		}
		if current.Revision != request.ExpectedRevision || !current.RevokedAt.IsZero() {
			return learning.ErrConflict
		}
		current.Revision++
		current.RevokedAt = s.clock.Now().UTC()
		current.RevocationReason = request.Reason
		digest, digestErr := learning.Digest(current)
		if digestErr != nil {
			return digestErr
		}
		result = learning.Publication{LessonID: current.ID, Revision: current.Revision, SourceID: current.Result.RunID, Operation: "revoke", Payload: current, PayloadDigest: digest, AggregateID: "learning/" + current.ID.String(), Actor: request.Actor}
		return repository.insertLessonPublication(tc, result)
	})
	if err != nil {
		return learning.Publication{}, err
	}
	return result, ctx.Err()
}

// GetLessonPublication reads one accepted payload with its latest delivery state.
func (s *Repository) GetLessonPublication(ctx context.Context, identifier id.ID, revision int64) (learning.Publication, error) {
	if identifier.IsNil() || identifier.Prefix() != learning.PrefixLesson || revision < 1 {
		return learning.Publication{}, learning.ErrInvalid
	}
	key := lessonRevisionKey(identifier, revision)
	row, err := s.get(ctx, Query{Kind: kindLessonPublication, ID: key})
	if errors.Is(err, ErrAbsent) {
		return learning.Publication{}, learning.ErrNotFound
	}
	if err != nil {
		return learning.Publication{}, err
	}
	if row.Deleted {
		return learning.Publication{}, learning.ErrIneligible
	}
	var publication learning.Publication
	if json.Unmarshal([]byte(row.Body), &publication) != nil || row.Deleted || row.ScopeCanon != cortex.ScopeFromContext(ctx).Canonical() || publication.Payload.Scope.Canonical() != row.ScopeCanon || publication.LessonID != identifier || publication.Revision != revision || publication.PayloadDigest != row.Digest {
		return learning.Publication{}, learning.ErrConflict
	}
	digest, err := learning.Digest(publication.Payload)
	if err != nil || digest != publication.PayloadDigest {
		return learning.Publication{}, learning.ErrConflict
	}
	states, err := s.driver.Select(ctx, Query{Kind: kindLessonDelivery, RunID: key, Reverse: true, Limit: 1})
	if err != nil {
		return learning.Publication{}, err
	}
	if len(states) != 0 {
		var state learning.Publication
		if json.Unmarshal([]byte(states[0].Body), &state) != nil || state.LessonID != identifier || state.Revision != revision || state.PayloadDigest != digest || state.Generation < 1 || states[0].Digest != evidence.Digest([]byte(states[0].Body)) {
			return learning.Publication{}, learning.ErrConflict
		}
		stateDigest, digestErr := learning.Digest(state.Payload)
		if digestErr != nil || stateDigest != digest || state.AggregateID != publication.AggregateID || state.SourceID != publication.SourceID || state.Operation != publication.Operation || state.Actor != publication.Actor {
			return learning.Publication{}, learning.ErrConflict
		}
		publication = state
	}
	return publication, ctx.Err()
}

// ListLessonPublications scans a bounded durable page; callers choose eligible work.
func (s *Repository) ListLessonPublications(ctx context.Context, after string, limit int) (learning.PublicationPage, error) {
	if limit < 1 || limit > 100 || len(after) > 128 {
		return learning.PublicationPage{}, learning.ErrInvalid
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindLessonPublication, After: after, Limit: limit})
	if err != nil {
		return learning.PublicationPage{}, err
	}
	page := learning.PublicationPage{Items: []learning.Publication{}}
	for _, row := range rows {
		page.After = row.ID
		if row.Deleted {
			continue
		}
		var base learning.Publication
		if json.Unmarshal([]byte(row.Body), &base) != nil {
			return learning.PublicationPage{}, learning.ErrConflict
		}
		current, readErr := s.GetLessonPublication(ctx, base.LessonID, base.Revision)
		if readErr != nil {
			return learning.PublicationPage{}, readErr
		}
		page.Items = append(page.Items, current)
		page.After = row.ID
	}
	if len(rows) < limit {
		page.After = ""
	}
	return page, ctx.Err()
}

func (s *Repository) correctLesson(ctx context.Context, ref learning.Ref, replacement learning.Publication, actor string) error {
	if ref.ID == replacement.LessonID || ref.Revision < 1 || ref.Revision == math.MaxInt64 || !evidence.ValidDigest(ref.Digest) {
		return learning.ErrInvalid
	}
	prior, err := s.GetLesson(ctx, ref.ID)
	if err != nil {
		return err
	}
	digest, err := learning.Digest(prior)
	if err != nil || digest != ref.Digest || prior.Revision != ref.Revision || !prior.RevokedAt.IsZero() || prior.Result == nil || prior.Domain != replacement.Payload.Domain {
		return learning.ErrConflict
	}
	driver, ok := s.driver.(LessonRetentionDriver)
	if !ok {
		return learning.ErrUnavailable
	}
	if fenceErr := driver.FenceLessonRevision(ctx, lessonRevisionKey(prior.ID, prior.Revision), digest); fenceErr != nil {
		return fenceErr
	}
	prior.Revision++
	prior.RevokedAt = s.clock.Now().UTC()
	prior.RevocationReason = "superseded"
	prior.SupersededBy = &learning.Ref{ID: replacement.LessonID, Revision: replacement.Revision, Digest: replacement.PayloadDigest}
	digest, err = learning.Digest(prior)
	if err != nil {
		return err
	}
	return s.insertLessonPublication(ctx, learning.Publication{LessonID: prior.ID, Revision: prior.Revision, SourceID: prior.Result.RunID, Operation: "revoke", Payload: prior, PayloadDigest: digest, AggregateID: "learning/" + prior.ID.String(), Actor: actor})
}
