package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func publicationDelivery(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("PublicationDeliveryFencesAndAcknowledgements", func(t *testing.T) {
		now := time.Now().UTC()
		clockFactory := func(t *testing.T, _ func() time.Time) store.Store { return factory(t, func() time.Time { return now }) }
		s, ctx, request := publishedLessonFixture(t, clockFactory)
		claim := optimization.PublicationClaim{LessonID: request.Lesson.ID, LocalRevision: 1, Owner: "publisher-a", Duration: time.Minute, Actor: request.Actor}
		first := claimPublication(ctx, t, s, claim)
		if first.Attempts != 1 || first.DeliveryLease.Generation != 1 || first.DeliveryLease.Owner != "publisher-a" {
			t.Fatalf("claim: %+v", first)
		}
		if err := s.Transact(ctx, func(tx store.Tx) error { _, e := tx.ClaimPublication(claim); return e }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("double claim: %v", err)
		}
		now = now.Add(2 * time.Minute)
		claim.Owner = "publisher-b"
		second := claimPublication(ctx, t, s, claim)
		receipt := optimization.PublicationReceipt{LessonID: second.LessonID, PayloadDigest: second.PayloadDigest, AggregateID: second.RemoteAggregateID, Version: 1, AcknowledgedAt: now}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CompletePublication(first.DeliveryLease, receipt) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("stale ack: %v", err)
		}
		bad := receipt
		bad.PayloadDigest = "wrong"
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CompletePublication(second.DeliveryLease, bad) }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("wrong digest: %v", err)
		}
		for range 2 {
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CompletePublication(second.DeliveryLease, receipt) }); err != nil {
				t.Fatal(err)
			}
		}
		saved, err := s.GetPublication(ctx, request.Lesson.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if saved.CompletedAt.IsZero() || saved.RemoteVersion != 1 || saved.Attempts != 2 {
			t.Fatalf("ack: %+v", saved)
		}
		page, err := s.ListPublications(ctx, optimization.PublicationQuery{PendingOnly: true})
		if err != nil || len(page.Items) != 0 {
			t.Fatalf("ack remains pending: %+v %v", page, err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { _, e := tx.ClaimPublication(claim); return e }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("completed dispatched: %v", err)
		}
	})
	t.Run("PublicationRetryDelayRollbackAndRevocation", func(t *testing.T) {
		now := time.Now().UTC()
		clockFactory := func(t *testing.T, _ func() time.Time) store.Store { return factory(t, func() time.Time { return now }) }
		s, ctx, request := publishedLessonFixture(t, clockFactory)
		claim := optimization.PublicationClaim{LessonID: request.Lesson.ID, LocalRevision: 1, Owner: "publisher", Duration: time.Minute, Actor: request.Actor}
		abort := errors.New("rollback")
		if err := s.Transact(ctx, func(tx store.Tx) error {
			_, e := tx.ClaimPublication(claim)
			if e != nil {
				return e
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		first := claimPublication(ctx, t, s, claim)
		if first.Attempts != 1 {
			t.Fatal("aborted claim survived")
		}
		retry := now.Add(5 * time.Minute)
		if err := s.Transact(ctx, func(tx store.Tx) error {
			return tx.FailPublication(first.DeliveryLease, "remote_unavailable", retry, false)
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Transact(ctx, func(tx store.Tx) error { _, e := tx.ClaimPublication(claim); return e }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("retry delay bypass: %v", err)
		}
		now = retry
		second := claimPublication(ctx, t, s, claim)
		if err := s.Transact(ctx, func(tx store.Tx) error {
			return tx.RevokeLesson(optimization.LessonRevocation{LessonID: request.Lesson.ID, ExpectedRevision: 1, Actor: request.Actor, Reason: "source revoked"})
		}); err != nil {
			t.Fatal(err)
		}
		receipt := optimization.PublicationReceipt{LessonID: request.Lesson.ID, PayloadDigest: second.PayloadDigest, AggregateID: second.RemoteAggregateID, Version: 1, AcknowledgedAt: now}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CompletePublication(second.DeliveryLease, receipt) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("superseded publish acknowledged: %v", err)
		}
		claim.LocalRevision = 2
		revocation := claimPublication(ctx, t, s, claim)
		if revocation.Operation != optimization.PublicationRevoke {
			t.Fatal("revocation lost")
		}
		foreign := scoped(optimizationScope("foreign-delivery"))
		if err := s.Transact(foreign, func(tx store.Tx) error {
			return tx.FailPublication(revocation.DeliveryLease, "remote_unavailable", now.Add(time.Minute), false)
		}); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("foreign delivery: %v", err)
		}
	})
}

func claimPublication(ctx context.Context, t *testing.T, s store.Store, request optimization.PublicationClaim) optimization.Publication {
	t.Helper()
	var value optimization.Publication
	if err := s.Transact(ctx, func(tx store.Tx) error { var e error; value, e = tx.ClaimPublication(request); return e }); err != nil {
		t.Fatal(err)
	}
	return value
}

// Keep the foreign scope structurally valid without sharing the producer's IDs.
func optimizationScope(value string) cortex.Scope {
	return cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: value}}}
}

type publicationSink struct {
	receipts map[string]optimization.PublicationReceipt
	fail     bool
}

func (s *publicationSink) Deliver(_ context.Context, p optimization.Publication) (optimization.PublicationReceipt, error) {
	if s.receipts == nil {
		s.receipts = map[string]optimization.PublicationReceipt{}
	}
	key := p.RemoteAggregateID
	receipt, found := s.receipts[key]
	if !found {
		receipt = optimization.PublicationReceipt{LessonID: p.LessonID, PayloadDigest: p.PayloadDigest, AggregateID: key, Version: 1, AcknowledgedAt: p.CreatedAt}
		s.receipts[key] = receipt
	}
	if s.fail {
		s.fail = false
		return optimization.PublicationReceipt{}, errors.New("lost remote acknowledgement")
	}
	return receipt, nil
}

type publicationAuthority struct {
	actor  optimization.IdentityRef
	denied bool
}

func (a *publicationAuthority) Authorize(context.Context, optimization.Action, optimization.Resource) error {
	if a.denied {
		return optimization.ErrPolicyDenied
	}
	return nil
}
func (a *publicationAuthority) Resolve(ctx context.Context, _ optimization.IdentityRef, _ cortex.Scope) (context.Context, error) {
	return ctx, nil
}
func (a *publicationAuthority) Current(context.Context) (optimization.IdentityRef, error) {
	return a.actor, nil
}

func publicationWorker(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("PublicationWorkerRecoversAmbiguousDelivery", func(t *testing.T) {
		now := time.Now().UTC()
		clockFactory := func(t *testing.T, _ func() time.Time) store.Store { return factory(t, func() time.Time { return now }) }
		s, ctx, request := publishedLessonFixture(t, clockFactory)
		authority := &publicationAuthority{actor: request.Actor}
		sink := &publicationSink{fail: true}
		config := optimization.PublisherConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Sink: sink, Now: func() time.Time { return now }}
		publisher, err := optimization.NewPublisher(config)
		if err != nil {
			t.Fatal(err)
		}
		before, err := s.GetExperiment(ctx, request.ExperimentID)
		if err != nil {
			t.Fatal(err)
		}
		if err = publisher.WorkOnce(ctx); !errors.Is(err, optimization.ErrUnavailable) {
			t.Fatalf("ambiguous delivery: %v", err)
		}
		failed, err := s.GetPublication(ctx, request.Lesson.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if !failed.CompletedAt.IsZero() || failed.Attempts != 1 || !failed.NextRetry.After(now) || failed.LastErrorCode != "remote_unavailable" {
			t.Fatalf("lost retry state: %+v", failed)
		}
		after, err := s.GetExperiment(ctx, request.ExperimentID)
		if err != nil {
			t.Fatal(err)
		}
		if after.Revision != before.Revision || after.DecisionID != before.DecisionID || after.State != before.State {
			t.Fatal("publication outage changed experiment decision")
		}
		now = failed.NextRetry
		publisher, err = optimization.NewPublisher(config)
		if err != nil {
			t.Fatal(err)
		}
		if err = publisher.WorkOnce(ctx); err != nil {
			t.Fatal(err)
		}
		done, err := s.GetPublication(ctx, request.Lesson.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if done.CompletedAt.IsZero() || done.RemoteVersion != 1 || done.Attempts != 2 {
			t.Fatalf("restart failed to acknowledge: %+v", done)
		}
		if err = publisher.WorkOnce(ctx); err != nil {
			t.Fatal(err)
		}
		// Verify the durable records, not a callback count: one remote version and one
		// local acknowledgement survive an ambiguous first response.
		if len(sink.receipts) != 1 || done.Receipt == nil || done.Receipt.PayloadDigest != done.PayloadDigest {
			t.Fatal("delivery evidence missing")
		}
	})
	t.Run("PublicationWorkerRechecksCurrentAuthority", func(t *testing.T) {
		s, ctx, request := publishedLessonFixture(t, factory)
		authority := &publicationAuthority{actor: request.Actor, denied: true}
		sink := &publicationSink{}
		worker, err := optimization.NewPublisher(optimization.PublisherConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Sink: sink})
		if err != nil {
			t.Fatal(err)
		}
		if err = worker.WorkOnce(ctx); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("revoked caller: %v", err)
		}
		saved, err := s.GetPublication(ctx, request.Lesson.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if saved.Attempts != 0 || len(sink.receipts) != 0 {
			t.Fatal("denied worker dispatched")
		}
	})
}

type publicationSinkFunc func(context.Context, optimization.Publication) (optimization.PublicationReceipt, error)

func (f publicationSinkFunc) Deliver(ctx context.Context, p optimization.Publication) (optimization.PublicationReceipt, error) {
	return f(ctx, p)
}

func publicationWorkerBoundaries(t *testing.T, factory Factory) {
	t.Helper()
	for _, scenario := range []string{"authority_revoked", "invalid_receipt", "cancelled", "exhausted", "lesson_revoked", "provider_policy_denied"} {
		t.Run("PublicationWorkerBoundary/"+scenario, func(t *testing.T) {
			now := time.Now().UTC()
			clockFactory := func(t *testing.T, _ func() time.Time) store.Store { return factory(t, func() time.Time { return now }) }
			s, base, request := publishedLessonFixture(t, clockFactory)
			ctx, cancel := context.WithCancel(base)
			defer cancel()
			authority := &publicationAuthority{actor: request.Actor}
			sink := publicationSinkFunc(func(_ context.Context, p optimization.Publication) (optimization.PublicationReceipt, error) {
				receipt := optimization.PublicationReceipt{LessonID: p.LessonID, PayloadDigest: p.PayloadDigest, AggregateID: p.RemoteAggregateID, Version: 1, AcknowledgedAt: now}
				switch scenario {
				case "authority_revoked":
					authority.denied = true
				case "invalid_receipt":
					receipt.PayloadDigest = "forged"
				case "cancelled":
					cancel()
				case "provider_policy_denied":
					return optimization.PublicationReceipt{}, optimization.ErrPolicyDenied
				case "exhausted":
					return optimization.PublicationReceipt{}, errors.New("private destination failure")
				case "lesson_revoked":
					if err := s.Transact(base, func(tx store.Tx) error {
						return tx.RevokeLesson(optimization.LessonRevocation{LessonID: p.LessonID, ExpectedRevision: 1, Actor: request.Actor, Reason: "withdrawn during delivery"})
					}); err != nil {
						t.Fatal(err)
					}
				}
				return receipt, nil
			})
			worker, err := optimization.NewPublisher(optimization.PublisherConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Sink: sink, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			attempts := 1
			if scenario == "exhausted" {
				attempts = 8
			}
			for range attempts {
				err = worker.WorkOnce(ctx)
				switch scenario {
				case "authority_revoked":
					if !errors.Is(err, optimization.ErrPolicyDenied) {
						t.Fatalf("revoked authority: %v", err)
					}
				case "cancelled":
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("cancelled delivery: %v", err)
					}
				case "lesson_revoked":
					if err != nil {
						t.Fatal(err)
					}
				default:
					if !errors.Is(err, optimization.ErrUnavailable) {
						t.Fatalf("failed delivery: %v", err)
					}
				}
				saved, getErr := s.GetPublication(base, request.Lesson.ID, 1)
				if getErr != nil {
					t.Fatal(getErr)
				}
				if !saved.CompletedAt.IsZero() || saved.Receipt != nil {
					t.Fatal("failed delivery acquired acknowledgement")
				}
				if !saved.NextRetry.IsZero() {
					now = saved.NextRetry
				}
			}
			saved, err := s.GetPublication(base, request.Lesson.ID, 1)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "provider_policy_denied" && saved.TerminalError != "policy_denied" {
				t.Fatalf("policy denial lost: %+v", saved)
			}
			if scenario == "invalid_receipt" && saved.TerminalError != "invalid_receipt" {
				t.Fatalf("invalid acknowledgement not quarantined: %+v", saved)
			}
			if scenario == "exhausted" {
				if saved.Attempts != 8 || saved.TerminalError != "remote_unavailable" {
					t.Fatalf("unbounded retries: %+v", saved)
				}
				if err = worker.WorkOnce(ctx); err != nil {
					t.Fatal(err)
				}
				final, getErr := s.GetPublication(base, request.Lesson.ID, 1)
				if getErr != nil || final.Attempts != 8 {
					t.Fatalf("terminal retried: %+v %v", final, getErr)
				}
			}
		})
	}
}

func publicationLeaseExpiryBound(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("PublicationWorkerBoundsExpiredAttempts", func(t *testing.T) {
		now := time.Now().UTC()
		s, ctx, request := publishedLessonFixture(t, func(t *testing.T, _ func() time.Time) store.Store { return factory(t, func() time.Time { return now }) })
		authority := &publicationAuthority{actor: request.Actor}
		dispatched := 0
		sink := publicationSinkFunc(func(_ context.Context, p optimization.Publication) (optimization.PublicationReceipt, error) {
			dispatched++
			now = p.DeliveryLease.ExpiresAt
			return optimization.PublicationReceipt{}, errors.New("lost worker")
		})
		config := optimization.PublisherConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Sink: sink, Now: func() time.Time { return now }}
		for range 9 {
			worker, err := optimization.NewPublisher(config)
			if err != nil {
				t.Fatal(err)
			}
			if err = worker.WorkOnce(ctx); err != nil && !errors.Is(err, optimization.ErrUnavailable) {
				t.Fatal(err)
			}
		}
		saved, err := s.GetPublication(ctx, request.Lesson.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if dispatched != 8 || saved.TerminalError != "remote_unavailable" {
			t.Fatalf("expired attempts bypassed bound: dispatches=%d terminal=%q", dispatched, saved.TerminalError)
		}
	})
}

func publicationClaimRace(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("PublicationCompetingClaims", func(t *testing.T) {
		s, ctx, request := publishedLessonFixture(t, factory)
		start := make(chan struct{})
		results := make(chan error, 2)
		for _, owner := range []string{"worker-one", "worker-two"} {
			go func() {
				<-start
				results <- s.Transact(ctx, func(tx store.Tx) error {
					_, err := tx.ClaimPublication(optimization.PublicationClaim{LessonID: request.Lesson.ID, LocalRevision: 1, Owner: owner, Duration: time.Minute, Actor: request.Actor})
					return err
				})
			}()
		}
		close(start)
		wins := 0
		for range 2 {
			err := <-results
			if err == nil {
				wins++
			} else if !errors.Is(err, optimization.ErrConflict) {
				t.Fatal(err)
			}
		}
		saved, err := s.GetPublication(ctx, request.Lesson.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		if wins != 1 || saved.Attempts != 1 {
			t.Fatalf("competing claims: wins=%d attempts=%d", wins, saved.Attempts)
		}
	})
}
