package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

// PublicationRecovery checks persisted claim takeover and acknowledgement after
// closing and reopening a database. Open must return the same durable database.
func PublicationRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	now := time.Now().UTC()
	factory := func(t *testing.T, _ func() time.Time) store.Store { return open(t, func() time.Time { return now }) }
	first, ctx, request := publishedLessonFixture(t, factory)
	claim := optimization.PublicationClaim{LessonID: request.Lesson.ID, LocalRevision: 1, Owner: "old-publisher", Duration: time.Minute, Actor: request.Actor}
	prior := claimPublication(ctx, t, first, claim)
	// The destination committed, but the process stopped before local acknowledgement.
	sink := &publicationSink{}
	receipt, err := sink.Deliver(ctx, prior)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second := open(t, func() time.Time { return now })
	authority := &publicationAuthority{actor: request.Actor}
	worker, err := optimization.NewPublisher(optimization.PublisherConfig{Repository: second, Authorizer: authority, IdentityResolver: authority, Caller: authority, Sink: sink, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	if err = worker.WorkOnce(ctx); err != nil {
		t.Fatal(err)
	}
	pending, err := second.GetPublication(ctx, request.Lesson.ID, 1)
	if err != nil || pending.Attempts != 1 || !pending.CompletedAt.IsZero() {
		t.Fatalf("live lease was bypassed: %+v %v", pending, err)
	}
	now = prior.DeliveryLease.ExpiresAt
	if err = worker.WorkOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if err = second.Transact(ctx, func(tx store.Tx) error { return tx.CompletePublication(prior.DeliveryLease, receipt) }); !errors.Is(err, optimization.ErrConflict) {
		t.Fatalf("old process acknowledged: %v", err)
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
	third := open(t, func() time.Time { return now })
	saved, err := third.GetPublication(ctx, request.Lesson.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Attempts != 2 || saved.DeliveryLease.Generation != 2 || saved.CompletedAt.IsZero() || saved.Receipt == nil || *saved.Receipt != receipt || len(sink.receipts) != 1 {
		t.Fatalf("reopened delivery: %+v", saved)
	}
	page, err := third.ListPublications(ctx, optimization.PublicationQuery{PendingOnly: true})
	if err != nil || len(page.Items) != 0 {
		t.Fatalf("completed redelivery: %+v %v", page, err)
	}
	events, err := third.ListEvents(ctx, request.ExperimentID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	claimed, delivered := 0, 0
	for _, event := range events.Items {
		switch event.Type {
		case "lesson_publication_claimed":
			claimed++
		case "lesson_publication_delivered":
			delivered++
		}
	}
	if claimed != 2 || delivered != 1 {
		t.Fatalf("durable delivery audit: claims=%d acknowledgements=%d", claimed, delivered)
	}
}
