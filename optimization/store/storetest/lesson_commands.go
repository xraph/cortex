package storetest

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

type lessonCommandAuthority struct {
	publicationAuthority
	resolveError error
	onResolve    func()
}

func (a *lessonCommandAuthority) Resolve(ctx context.Context, _ opt.IdentityRef, _ cortex.Scope) (context.Context, error) {
	if a.onResolve != nil {
		a.onResolve()
	}
	return ctx, a.resolveError
}
func lessonCommandService(t *testing.T, s store.Store, a *lessonCommandAuthority) *opt.Optimizer {
	t.Helper()
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: s, Authorizer: a, Caller: a, IdentityResolver: a, Handler: unusedPhase{}, Worker: opt.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func lessonCommands(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("LessonCommandReceiptsSurviveLaterCycles", func(t *testing.T) { lessonCommandRecovery(t, factory, false) })
	t.Run("LessonCommandCurrentAuthority", func(t *testing.T) {
		s, ctx, p := publishedLessonFixture(t, factory)
		a := &lessonCommandAuthority{publicationAuthority: publicationAuthority{actor: opt.IdentityRef{Subject: "operator"}}}
		service := lessonCommandService(t, s, a)
		claim := claimPublication(ctx, t, s, opt.PublicationClaim{LessonID: p.Lesson.ID, LocalRevision: 1, Owner: "worker", Duration: time.Minute, Actor: p.Actor})
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.FailPublication(claim.DeliveryLease, "conflict", time.Time{}, true) }); err != nil {
			t.Fatal(err)
		}
		retry := opt.RetryLessonRequest{LessonID: p.Lesson.ID, LocalRevision: 1, ExpectedGeneration: claim.DeliveryLease.Generation, Reason: "Destination repaired", IdempotencyKey: "retry"}
		for _, mutate := range []func(*opt.RetryLessonRequest){func(r *opt.RetryLessonRequest) { r.IdempotencyKey = "" }, func(r *opt.RetryLessonRequest) { r.LessonID = p.ExperimentID }, func(r *opt.RetryLessonRequest) { r.Reason = "" }, func(r *opt.RetryLessonRequest) { r.ExpectedGeneration = 0 }} {
			invalid := retry
			mutate(&invalid)
			if _, err := service.RetryLessonCommand(ctx, invalid); !errors.Is(err, opt.ErrInvalidSpec) {
				t.Fatalf("invalid retry: %v", err)
			}
		}
		a.denied = true
		if _, err := service.RetryLessonCommand(ctx, retry); !errors.Is(err, opt.ErrPolicyDenied) {
			t.Fatalf("denied retry: %v", err)
		}
		a.denied = false
		a.resolveError = opt.ErrPolicyDenied
		if _, err := service.RetryLessonCommand(ctx, retry); !errors.Is(err, opt.ErrPolicyDenied) {
			t.Fatalf("original requester denied: %v", err)
		}
		a.resolveError = nil
		a.onResolve = func() { a.actor.Subject = "changed-during-resolution" }
		if _, err := service.RetryLessonCommand(ctx, retry); !errors.Is(err, opt.ErrPolicyDenied) {
			t.Fatalf("reviewer changed: %v", err)
		}
		a.onResolve = nil
		a.actor.Subject = "operator"
		saved, err := s.GetPublication(ctx, p.Lesson.ID, 1)
		if err != nil || saved.LastRetry != nil {
			t.Fatal("denied command reset delivery")
		}
		a.resolveError = opt.ErrUnavailable
		revoke := opt.RevokeLessonRequest{LessonID: p.Lesson.ID, ExpectedRevision: 1, Reason: "Evidence withdrawn", IdempotencyKey: "revoke"}
		a.denied = true
		if _, err = service.RevokeLessonCommand(ctx, revoke); !errors.Is(err, opt.ErrPolicyDenied) {
			t.Fatalf("denied revocation: %v", err)
		}
		a.denied = false
		revoked, err := service.RevokeLessonCommand(ctx, revoke)
		if err != nil || revoked.RevokedAt.IsZero() {
			t.Fatalf("revoke depends on original requester: %+v %v", revoked, err)
		}
		a.denied = true
		if _, err = service.RevokeLessonCommand(ctx, revoke); !errors.Is(err, opt.ErrPolicyDenied) {
			t.Fatalf("revocation retry skipped current grant: %v", err)
		}
	})
}

// LessonCommandRecovery checks durable operator receipts after closing the store.
func LessonCommandRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	lessonCommandRecovery(t, open, true)
}

func lessonCommandRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store, reopen bool) {
	t.Helper()
	s, ctx, p := publishedLessonFixture(t, open)
	a := &lessonCommandAuthority{publicationAuthority: publicationAuthority{actor: opt.IdentityRef{Subject: "operator"}}}
	service := lessonCommandService(t, s, a)
	var first, second opt.RetryLessonRequest
	for index := range 2 {
		claim := claimPublication(ctx, t, s, opt.PublicationClaim{LessonID: p.Lesson.ID, LocalRevision: 1, Owner: "worker", Duration: time.Minute, Actor: p.Actor})
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.FailPublication(claim.DeliveryLease, "conflict", time.Time{}, true) }); err != nil {
			t.Fatal(err)
		}
		request := opt.RetryLessonRequest{LessonID: p.Lesson.ID, LocalRevision: 1, ExpectedGeneration: claim.DeliveryLease.Generation, Reason: "Destination repaired", IdempotencyKey: []string{"first-secret-key", "second-secret-key"}[index]}
		if index == 0 {
			abort := errors.New("abort retry")
			change := opt.PublicationRetry{LessonID: request.LessonID, LocalRevision: request.LocalRevision, ExpectedGeneration: request.ExpectedGeneration, Reason: request.Reason, Actor: a.actor, CommandKey: fmt.Sprintf("operator/%x", sha256.Sum256([]byte(request.IdempotencyKey)))}
			if err := s.Transact(ctx, func(tx store.Tx) error {
				if e := tx.RetryPublication(change); e != nil {
					return e
				}
				return abort
			}); !errors.Is(err, abort) {
				t.Fatal(err)
			}
			unchanged, e := s.GetPublication(ctx, p.Lesson.ID, 1)
			if e != nil || unchanged.LastRetry != nil || unchanged.TerminalError == "" {
				t.Fatal("aborted command persisted")
			}
		}
		// Competing instances may observe a bounded database fence conflict.
		// Replaying after contention must recover the one committed command.
		outcomes := make(chan error, 3)
		for range 3 {
			go func() { _, e := service.RetryLessonCommand(ctx, request); outcomes <- e }()
		}
		for range 3 {
			if e := <-outcomes; e != nil && !errors.Is(e, opt.ErrConflict) {
				t.Fatal(e)
			}
		}
		retried, err := service.RetryLessonCommand(ctx, request)
		if err != nil || retried.LastRetry == nil || retried.RetryAttemptBase != index+1 {
			t.Fatalf("retry: %+v %v", retried, err)
		}
		if index == 0 {
			first = request
		} else {
			second = request
		}
	}
	if reopen {
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		s = open(t, time.Now)
		service = lessonCommandService(t, s, a)
	}
	before, err := s.GetPublication(ctx, p.Lesson.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := service.RetryLessonCommand(ctx, first)
	if err != nil || !reflect.DeepEqual(before, recovered) {
		t.Fatalf("old retry changed later cycle: %+v %v", recovered, err)
	}
	changed := first
	changed.Reason = "different request"
	if _, err = service.RetryLessonCommand(ctx, changed); !errors.Is(err, opt.ErrConflict) {
		t.Fatalf("changed retry: %v", err)
	}
	changed = first
	changed.ExpectedGeneration = second.ExpectedGeneration
	if _, err = service.RetryLessonCommand(ctx, changed); !errors.Is(err, opt.ErrConflict) {
		t.Fatalf("key reused for new cycle: %v", err)
	}
	a.actor.Subject = "another-operator"
	if _, err = service.RetryLessonCommand(ctx, first); !errors.Is(err, opt.ErrConflict) {
		t.Fatalf("changed actor: %v", err)
	}
	a.actor.Subject = "operator"
	foreign := cortex.WithScope(ctx, optimizationScope("foreign"))
	if _, err = service.RetryLessonCommand(foreign, first); !errors.Is(err, opt.ErrNotFound) {
		t.Fatalf("foreign retry: %v", err)
	}
	revoke := opt.RevokeLessonRequest{LessonID: p.Lesson.ID, ExpectedRevision: 1, Reason: "Source withdrawn", IdempotencyKey: first.IdempotencyKey}
	if _, err = service.RevokeLessonCommand(ctx, revoke); !errors.Is(err, opt.ErrConflict) {
		t.Fatalf("key reused across actions: %v", err)
	}
	revoke.IdempotencyKey = "revoke-secret-key"
	revoked, err := service.RevokeLessonCommand(ctx, revoke)
	if err != nil || revoked.Revision != 2 || revoked.RevokedAt.IsZero() {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	if reopen {
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
		s = open(t, time.Now)
		service = lessonCommandService(t, s, a)
	}
	again, err := service.RevokeLessonCommand(ctx, revoke)
	if err != nil || !reflect.DeepEqual(again, revoked) {
		t.Fatalf("recovered revoke: %+v %v", again, err)
	}
	if _, err = service.RetryLessonCommand(ctx, first); err != nil {
		t.Fatalf("old retry after withdrawal: %v", err)
	}
	old, err := s.GetPublication(ctx, p.Lesson.ID, 1)
	if err != nil || old.SupersededAt.IsZero() || old.RetryAttemptBase != 2 {
		t.Fatal("old command revived publication")
	}
	withdrawal, err := s.GetPublication(ctx, p.Lesson.ID, 2)
	if err != nil || withdrawal.Operation != opt.PublicationRevoke || withdrawal.Attempts != 0 {
		t.Fatal("revocation did remote work or lost outbox")
	}
	events, err := s.ListEvents(ctx, p.ExperimentID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	retries, revokes := 0, 0
	for _, event := range events.Items {
		if event.Type == "lesson_publication_retry_requested" {
			retries++
		}
		if event.Type == "lesson_revoked" {
			revokes++
		}
		if strings.Contains(string(event.Payload), "secret-key") {
			t.Fatal("raw transport key persisted in event")
		}
	}
	if retries != 2 || revokes != 1 {
		t.Fatalf("duplicate command events: retries=%d revokes=%d", retries, revokes)
	}
	a.denied = true
	if _, err = service.RetryLessonCommand(ctx, first); !errors.Is(err, opt.ErrPolicyDenied) {
		t.Fatalf("retry skipped current permission: %v", err)
	}
	a.denied = false
	revoke.LessonID = id.New(opt.PrefixLesson)
	if _, err = service.RevokeLessonCommand(ctx, revoke); !errors.Is(err, opt.ErrNotFound) {
		t.Fatalf("unknown lesson: %v", err)
	}
}
