package storetest

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func publicationRetry(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("PublicationOperatorRetryPreservesHistory", func(t *testing.T) {
		s, ctx, request := publishedLessonFixture(t, factory)
		claim := claimPublication(ctx, t, s, optimization.PublicationClaim{LessonID: request.Lesson.ID, LocalRevision: 1, Owner: "publisher", Duration: time.Minute, Actor: request.Actor})
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.FailPublication(claim.DeliveryLease, "conflict", time.Time{}, true) }); err != nil {
			t.Fatal(err)
		}
		retry := optimization.PublicationRetry{LessonID: request.Lesson.ID, LocalRevision: 1, ExpectedGeneration: claim.DeliveryLease.Generation, Actor: optimization.IdentityRef{Subject: "operator"}, Reason: "destination repaired"}
		for range 2 {
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.RetryPublication(retry) }); err != nil {
				t.Fatal(err)
			}
		}
		saved, err := s.GetPublication(ctx, request.Lesson.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Attempts != 1 || saved.RetryAttemptBase != 1 || saved.TerminalError != "" || !saved.NextRetry.IsZero() || saved.PayloadDigest != claim.PayloadDigest || saved.LastRetry == nil || *saved.LastRetry != retry {
			t.Fatalf("retry lost history: %+v", saved)
		}
		events, err := s.ListEvents(ctx, request.ExperimentID, 0, 500)
		if err != nil {
			t.Fatal(err)
		}
		retries := 0
		for _, event := range events.Items {
			if event.Type == "lesson_publication_retry_requested" {
				retries++
				if event.Actor != retry.Actor {
					t.Fatal("retry actor not audited")
				}
				var audited optimization.PublicationRetry
				if decodeErr := json.Unmarshal(event.Payload, &audited); decodeErr != nil || audited != retry {
					t.Fatalf("retry reason and fence not audited: %+v %v", audited, decodeErr)
				}
			}
		}
		if retries != 1 {
			t.Fatalf("duplicate retry audit: %d", retries)
		}
		next := claimPublication(ctx, t, s, optimization.PublicationClaim{LessonID: request.Lesson.ID, LocalRevision: 1, Owner: "replacement", Duration: time.Minute, Actor: retry.Actor})
		if next.Attempts != 2 || next.DeliveryLease.Generation != 2 {
			t.Fatal("retry reset the fence or attempt history")
		}
		// A lost retry response must not reset the budget again after work resumes.
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RetryPublication(retry) }); err != nil {
			t.Fatal(err)
		}
		retry.Reason = "different request"
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RetryPublication(retry) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("stale retry admitted: %v", err)
		}
	})
	t.Run("PublicationOperatorRetryRefusesInvalidState", func(t *testing.T) {
		s, ctx, request := publishedLessonFixture(t, factory)
		claim := claimPublication(ctx, t, s, optimization.PublicationClaim{LessonID: request.Lesson.ID, LocalRevision: 1, Owner: "publisher", Duration: time.Minute, Actor: request.Actor})
		retry := optimization.PublicationRetry{LessonID: request.Lesson.ID, LocalRevision: 1, ExpectedGeneration: 1, Actor: request.Actor, Reason: "retry"}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.RetryPublication(retry) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("active delivery interrupted: %v", err)
		}
		if err := s.Transact(ctx, func(tx store.Tx) error {
			return tx.FailPublication(claim.DeliveryLease, "remote_unavailable", time.Time{}, true)
		}); err != nil {
			t.Fatal(err)
		}
		aborted := errors.New("abort retry")
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if retryErr := tx.RetryPublication(retry); retryErr != nil {
				return retryErr
			}
			return aborted
		}); !errors.Is(err, aborted) {
			t.Fatal(err)
		}
		saved, err := s.GetPublication(ctx, request.Lesson.ID, 1)
		if err != nil || saved.LastRetry != nil || saved.TerminalError == "" {
			t.Fatal("aborted retry persisted")
		}
		if err = s.Transact(ctx, func(tx store.Tx) error {
			return tx.RevokeLesson(optimization.LessonRevocation{LessonID: request.Lesson.ID, ExpectedRevision: 1, Actor: request.Actor, Reason: "withdrawn"})
		}); err != nil {
			t.Fatal(err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RetryPublication(retry) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("superseded delivery retried: %v", err)
		}
		if err = s.Transact(scoped(optimizationScope("foreign")), func(tx store.Tx) error { return tx.RetryPublication(retry) }); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("foreign retry: %v", err)
		}
	})
}

type retrySink struct{ unavailable bool }

func (s *retrySink) Deliver(_ context.Context, p optimization.Publication) (optimization.PublicationReceipt, error) {
	if s.unavailable {
		return optimization.PublicationReceipt{}, optimization.ErrUnavailable
	}
	return optimization.PublicationReceipt{LessonID: p.LessonID, PayloadDigest: p.PayloadDigest, AggregateID: p.RemoteAggregateID, Version: 1, AcknowledgedAt: p.CreatedAt}, nil
}

func publicationRetryWorker(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("PublicationOperatorRetryRenewsBoundedDelivery", func(t *testing.T) {
		now := time.Now().UTC()
		s, ctx, request := publishedLessonFixture(t, func(t *testing.T, _ func() time.Time) store.Store { return factory(t, func() time.Time { return now }) })
		authority := &publicationAuthority{actor: request.Actor}
		sink := &retrySink{unavailable: true}
		worker, err := optimization.NewPublisher(optimization.PublisherConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Sink: sink, Now: func() time.Time { return now }})
		if err != nil {
			t.Fatal(err)
		}
		for range 8 {
			if err = worker.WorkOnce(ctx); !errors.Is(err, optimization.ErrUnavailable) {
				t.Fatal(err)
			}
			saved, getErr := s.GetPublication(ctx, request.Lesson.ID, 1)
			if getErr != nil {
				t.Fatal(getErr)
			}
			if !saved.NextRetry.IsZero() {
				now = saved.NextRetry
			}
		}
		failed, err := s.GetPublication(ctx, request.Lesson.ID, 1)
		if err != nil || failed.TerminalError == "" {
			t.Fatalf("exhaustion: %+v %v", failed, err)
		}
		retry := optimization.PublicationRetry{LessonID: request.Lesson.ID, LocalRevision: 1, ExpectedGeneration: failed.DeliveryLease.Generation, Reason: "destination restored", Actor: optimization.IdentityRef{Subject: "forged"}}
		authority.denied = true
		if _, err = worker.Retry(ctx, retry); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("denied operator: %v", err)
		}
		authority.denied = false
		if _, err = worker.Retry(ctx, retry); err != nil {
			t.Fatal(err)
		}
		sink.unavailable = false
		if err = worker.WorkOnce(ctx); err != nil {
			t.Fatal(err)
		}
		done, err := s.GetPublication(ctx, request.Lesson.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if done.CompletedAt.IsZero() || done.Attempts != 9 || done.RetryAttemptBase != 8 || done.LastRetry.Actor != request.Actor {
			t.Fatalf("operator retry: %+v", done)
		}
	})
}

type retryAuthority struct {
	publicationAuthority
	denyRetry bool
}

func (a *retryAuthority) Authorize(ctx context.Context, action optimization.Action, resource optimization.Resource) error {
	if a.denyRetry && action == optimization.ActionRetryPublication {
		return optimization.ErrPolicyDenied
	}
	return a.publicationAuthority.Authorize(ctx, action, resource)
}

func publicationRetryPermission(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("PublicationRetryRequiresOperatorPermission", func(t *testing.T) {
		s, ctx, request := publishedLessonFixture(t, factory)
		claim := claimPublication(ctx, t, s, optimization.PublicationClaim{LessonID: request.Lesson.ID, LocalRevision: 1, Owner: "publisher", Duration: time.Minute, Actor: request.Actor})
		if err := s.Transact(ctx, func(tx store.Tx) error {
			return tx.FailPublication(claim.DeliveryLease, "remote_unavailable", time.Time{}, true)
		}); err != nil {
			t.Fatal(err)
		}
		authority := &retryAuthority{publicationAuthority: publicationAuthority{actor: request.Actor}, denyRetry: true}
		worker, err := optimization.NewPublisher(optimization.PublisherConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Sink: &retrySink{}})
		if err != nil {
			t.Fatal(err)
		}
		retry := optimization.PublicationRetry{LessonID: request.Lesson.ID, LocalRevision: 1, ExpectedGeneration: 1, Reason: "repaired"}
		if _, err = worker.Retry(ctx, retry); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("publish permission granted retry: %v", err)
		}
		failed, err := s.GetPublication(ctx, request.Lesson.ID, 1)
		if err != nil || failed.TerminalError == "" || failed.LastRetry != nil {
			t.Fatal("denied retry changed state")
		}
		authority.denyRetry = false
		if _, err = worker.Retry(ctx, retry); err != nil {
			t.Fatal(err)
		}
	})
}
