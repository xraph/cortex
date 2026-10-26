package operator_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/memory"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestLessonCommandAdapters(t *testing.T) {
	repo, ctx, p := storetest.PublishedLessonFixture(t, func(t *testing.T, now func() time.Time) store.Store {
		s, err := memory.New(memory.WithClock(now))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
	auth := &authority{}
	service, err := opt.NewOptimizer(opt.OptimizerConfig{Repository: repo, Authorizer: auth, Caller: auth, IdentityResolver: auth, Handler: auth, Worker: opt.DefaultWorkerConfig()})
	if err != nil {
		t.Fatal(err)
	}
	commands := operator.Commands{Optimizer: service}
	var claim opt.Publication
	if err = repo.Transact(ctx, func(tx store.Tx) error {
		var e error
		claim, e = tx.ClaimPublication(opt.PublicationClaim{LessonID: p.Lesson.ID, LocalRevision: 1, Owner: "worker", Actor: p.Actor, Duration: time.Minute})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	if err = repo.Transact(ctx, func(tx store.Tx) error { return tx.FailPublication(claim.DeliveryLease, "conflict", time.Time{}, true) }); err != nil {
		t.Fatal(err)
	}
	retry := operator.RetryLessonInput{ID: p.Lesson.ID.String(), LocalRevision: 1, ExpectedGeneration: 1, Reason: "Destination repaired"}
	revoke := operator.RevokeLessonInput{ID: retry.ID, ExpectedRevision: 1, Reason: "Source withdrawn"}
	var forged operator.RetryLessonInput
	if err = json.Unmarshal([]byte(`{"id":"`+retry.ID+`","local_revision":1,"expected_generation":1,"reason":"repaired","idempotency_key":"forged"}`), &forged); err != nil {
		t.Fatal(err)
	}
	if _, err = commands.RetryLesson(ctx, forged, ""); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("body supplied key: %v", err)
	}
	if _, err = commands.RevokeLesson(ctx, revoke, ""); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("missing revoke key: %v", err)
	}
	bad := retry
	bad.ID = p.ExperimentID.String()
	if _, err = commands.RetryLesson(ctx, bad, "retry"); !errors.Is(err, opt.ErrInvalidSpec) {
		t.Fatalf("wrong ID: %v", err)
	}
	retried, err := commands.RetryLesson(ctx, retry, "retry")
	if err != nil || retried.LastRetry == nil {
		t.Fatalf("retry: %+v %v", retried, err)
	}
	revoked, err := commands.RevokeLesson(ctx, revoke, "revoke")
	if err != nil || revoked.RevokedAt.IsZero() {
		t.Fatalf("revoke: %+v %v", revoked, err)
	}
	if _, err = commands.RetryLesson(context.Background(), retry, "retry"); err == nil {
		t.Fatal("unscoped command admitted")
	}
	if _, err = (operator.Commands{}).RetryLesson(ctx, retry, "retry"); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("missing optimizer: %v", err)
	}
	if _, err = (operator.Commands{}).RevokeLesson(ctx, revoke, "revoke"); !errors.Is(err, opt.ErrUnavailable) {
		t.Fatalf("missing optimizer: %v", err)
	}
}
