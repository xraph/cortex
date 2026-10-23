package storetest

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func publications(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ReflectionCompletionAtomic", func(t *testing.T) {
		s, ctx, bundle, lease, decision := decisionFixture(t, factory)
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
		current, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		request := publicationRequest(bundle, lease, decision)
		change := optimization.Transition{ExperimentID: current.ID, ExpectedRevision: current.Revision, NextState: optimization.StateReviewRequired, Lease: lease, IdempotencyKey: "reflection-complete", Actor: bundle.Experiment.Spec.Requester, Publications: []optimization.PublicationRequest{request}}
		abort := errors.New("abort reflection finish")
		if err = s.Transact(ctx, func(tx store.Tx) error {
			if commitErr := tx.CommitTransition(change); commitErr != nil {
				return commitErr
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if _, err = s.GetPublication(ctx, request.Lesson.ID, 1); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("aborted publication persisted: %v", err)
		}
		for range 2 {
			if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err != nil {
				t.Fatal(err)
			}
		}
		final, err := s.GetExperiment(ctx, current.ID)
		if err != nil || final.State != optimization.StateReviewRequired {
			t.Fatalf("reflection state: %+v %v", final, err)
		}
		if _, err = s.GetPublication(ctx, request.Lesson.ID, 1); err != nil {
			t.Fatal(err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RenewLease(lease) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("completed phase lease remained valid: %v", err)
		}
	})
	t.Run("RevokedLessonInvalidatesActivation", func(t *testing.T) {
		s, ctx, request := publishedLessonFixture(t, factory)
		reuse := func(*testing.T, func() time.Time) store.Store { return s }
		_, _, bundle, lease, decision := decisionFixture(t, reuse, func(bundle *optimization.ExperimentBundle) {
			bundle.Experiment.LessonIDs = []id.ID{request.Lesson.ID}
			bundle.Experiment.Spec.LessonCutoff = time.Now().Add(time.Second)
			bundle.Experiment.Spec.Objective.MinValidationCases = 1
			bundle.Experiment.Spec.Objective.RequireReproducibility = false
			bundle.Experiment.Spec.Evaluation = optimization.ContentRef{Ref: "consumer-plan", Revision: "1", Digest: strings.Repeat("e", 64)}
			for i, task := range bundle.Experiment.Spec.Tasks {
				task.Input += " consumer"
				sealed, err := optimization.SealTaskCase(task)
				if err != nil {
					t.Fatal(err)
				}
				bundle.Experiment.Spec.Tasks[i] = sealed
			}
			bundle.Experiment.EvaluationPlan = fixtureEvaluationPlan(bundle.Experiment.Spec)
		})
		decision.Outcome, decision.EvidenceClass = optimization.DecisionRecommend, optimization.EvidenceValidated
		decision.Comparison.Complete, decision.Comparison.MissingReasons = true, nil
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
		change := activationRequest(bundle, decision)
		active := activate(ctx, t, s, change)
		child := publicationRequest(bundle, lease, decision)
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.PutPublication(child) }); err != nil {
			t.Fatal(err)
		}
		published, err := s.GetPublication(ctx, child.Lesson.ID, 1)
		if err != nil {
			t.Fatal(err)
		}
		ref := optimization.LessonRef{ID: child.Lesson.ID, Revision: 1, Digest: published.PayloadDigest}
		if resolved, resolveErr := s.ResolveLesson(ctx, ref); resolveErr != nil || resolved.ID != child.Lesson.ID {
			t.Fatalf("eligible lesson unavailable: %v", resolveErr)
		}
		revoke := optimization.LessonRevocation{LessonID: request.Lesson.ID, ExpectedRevision: 1, Actor: optimization.IdentityRef{Subject: "reviewer"}, Reason: "source no longer valid"}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.RevokeLesson(revoke) }); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetActivation(ctx, change.AgentID, change.Channel); !errors.Is(err, optimization.ErrStaleVersion) {
			t.Fatalf("revoked active version still usable: %v", err)
		}
		if history, err := s.GetActivationGeneration(ctx, active.ID); err != nil || history.ID != active.ID {
			t.Fatal("revocation erased history")
		}
		if _, resolveErr := s.ResolveLesson(ctx, ref); !errors.Is(resolveErr, optimization.ErrStaleVersion) {
			t.Fatalf("transitive revoked source resolved: %v", resolveErr)
		}
		if retryErr := s.Transact(ctx, func(tx store.Tx) error { return tx.PutPublication(child) }); !errors.Is(retryErr, optimization.ErrStaleVersion) {
			t.Fatalf("invalidated publication retry accepted: %v", retryErr)
		}
		change.ID, change.ExpectedGeneration = id.New(optimization.PrefixActivation), 1
		if err := s.Transact(ctx, func(tx store.Tx) error { _, err := tx.CompareAndSwapActivation(change); return err }); !errors.Is(err, optimization.ErrStaleVersion) {
			t.Fatalf("revoked recommendation activated: %v", err)
		}
	})
	t.Run("LessonExpiryFencesStartClaimAndRenewal", func(t *testing.T) {
		now := time.Now()
		clockFactory := func(t *testing.T, _ func() time.Time) store.Store { return factory(t, func() time.Time { return now }) }
		s, ctx, bundle, lease, decision := decisionFixture(t, clockFactory)
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
		request := publicationRequest(bundle, lease, decision)
		request.Lesson.ExpiresAt = now.Add(10 * time.Second)
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.PutPublication(request) }); err != nil {
			t.Fatal(err)
		}
		newConsumer := func() optimization.ExperimentBundle {
			consumer := bundle
			consumer.Experiment.ID = id.New(optimization.PrefixExperiment)
			consumer.Experiment.LessonIDs = []id.ID{request.Lesson.ID}
			consumer.Experiment.Spec.LessonCutoff = now
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(consumer) }); err != nil {
				t.Fatal(err)
			}
			return consumer
		}
		draft, queued, running := newConsumer(), newConsumer(), newConsumer()
		queue := func(consumer optimization.ExperimentBundle) optimization.Transition {
			return optimization.Transition{ExperimentID: consumer.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: consumer.Experiment.Spec.Requester}
		}
		commitTrialChange(ctx, t, s, queue(queued))
		commitTrialChange(ctx, t, s, queue(running))
		var activeLease optimization.WorkLease
		if err := s.Transact(ctx, func(tx store.Tx) error {
			var err error
			activeLease, err = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: running.Experiment.ID, Owner: "worker", Duration: time.Minute})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(20 * time.Second)
		for _, action := range []struct {
			name string
			call func(store.Tx) error
		}{
			{"start", func(tx store.Tx) error { return tx.CommitTransition(queue(draft)) }},
			{"claim", func(tx store.Tx) error {
				_, err := tx.ClaimWork(optimization.ClaimRequest{ExperimentID: queued.Experiment.ID, Owner: "worker", Duration: time.Minute})
				return err
			}},
			{"renew", func(tx store.Tx) error { return tx.RenewLease(activeLease) }},
		} {
			t.Run(action.name, func(t *testing.T) {
				if err := s.Transact(ctx, action.call); !errors.Is(err, optimization.ErrStaleVersion) {
					t.Fatalf("expired lesson allowed %s: %v", action.name, err)
				}
			})
		}
	})
	t.Run("PublicationAtomicWithDecision", func(t *testing.T) {
		s, ctx, bundle, lease, decision := decisionFixture(t, factory)
		request := publicationRequest(bundle, lease, decision)
		change := decisionChange(ctx, t, s, bundle, lease, decision)
		abort := errors.New("abort reflection")
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if err := tx.CommitTransition(change); err != nil {
				return err
			}
			if err := tx.PutPublication(request); err != nil {
				return err
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if _, err := s.GetLesson(ctx, request.Lesson.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatal("aborted lesson remains")
		}
		if _, err := s.GetPublication(ctx, request.Lesson.ID, 1); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatal("aborted publication remains")
		}
		if _, err := s.GetDecision(ctx, decision.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatal("aborted decision remains")
		}
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if err := tx.CommitTransition(change); err != nil {
				return err
			}
			return tx.PutPublication(request)
		}); err != nil {
			t.Fatal(err)
		}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.PutPublication(request) }); err != nil {
			t.Fatalf("identical retry: %v", err)
		}
		lesson, err := s.GetLesson(ctx, request.Lesson.ID)
		if err != nil || lesson.Revision != 1 || lesson.CreatedAt.IsZero() || lesson.Statement != request.Lesson.Statement {
			t.Fatalf("lost lesson: %+v %v", lesson, err)
		}
		publication, err := s.GetPublication(ctx, lesson.ID, 1)
		if err != nil || publication.Operation != optimization.PublicationPublish || publication.LocalRevision != 1 || publication.PayloadDigest == "" || publication.RemoteAggregateID != request.RemoteAggregateID || publication.Payload.Revision != 1 || publication.Payload.CreatedAt != lesson.CreatedAt {
			t.Fatalf("lost publication: %+v %v", publication, err)
		}
		lesson.Applicability[0] = "changed"
		publication.Payload.Statement = "changed"
		again, err := s.GetLesson(ctx, lesson.ID)
		if err != nil || again.Applicability[0] == "changed" || again.Statement == "changed" {
			t.Fatal("caller changed local eligibility")
		}
		foreign := scoped(Fixture(t, "foreign-publication").Experiment.Spec.Scope)
		if _, err = s.GetLesson(foreign, lesson.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("lesson leaked scope: %v", err)
		}
		if _, err = s.GetPublication(foreign, lesson.ID, 1); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("publication leaked scope: %v", err)
		}
		request.Lesson.Statement = "changed"
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.PutPublication(request) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("changed retry accepted: %v", err)
		}
	})
	t.Run("PublicationRejectsInvalidEvidence", func(t *testing.T) {
		s, ctx, bundle, lease, decision := decisionFixture(t, factory)
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
		original := publicationRequest(bundle, lease, decision)
		for _, mode := range []string{"scope", "decision", "duplicate-decision", "trial", "validated", "contrastive", "schema", "expired", "actor", "lease", "revision", "remote", "statement"} {
			t.Run(mode, func(t *testing.T) {
				request := original
				switch mode {
				case "scope":
					request.Lesson.Scope = Fixture(t, "foreign").Experiment.Spec.Scope
				case "decision":
					request.Lesson.DecisionIDs = []id.ID{id.New(optimization.PrefixDecision)}
				case "duplicate-decision":
					request.Lesson.DecisionIDs = []id.ID{decision.ID, decision.ID}
				case "trial":
					request.Lesson.TrialIDs = []id.ID{id.New(optimization.PrefixTrial)}
				case "validated":
					request.Lesson.EvidenceClass = optimization.EvidenceValidated
				case "contrastive":
					request.Lesson.EvidenceClass = optimization.EvidenceContrastive
				case "schema":
					request.Lesson.HarnessSchema = 2
				case "expired":
					request.Lesson.ExpiresAt = time.Now().Add(-time.Hour)
				case "actor":
					request.Actor.Subject = " "
				case "lease":
					request.Lease.Generation++
				case "revision":
					request.Lesson.Revision = 7
				case "remote":
					request.RemoteAggregateID = ""
				case "statement":
					request.Lesson.Statement = " "
				}
				abort := errors.New("invalid publication accepted")
				err := s.Transact(ctx, func(tx store.Tx) error {
					if err := tx.PutPublication(request); err != nil {
						return err
					}
					return abort
				})
				if errors.Is(err, abort) {
					t.Fatal(mode)
				}
			})
		}
	})
	t.Run("RevocationOutboxAtomicAndMonotonic", func(t *testing.T) {
		s, ctx, request := publishedLessonFixture(t, factory)
		revoke := optimization.LessonRevocation{LessonID: request.Lesson.ID, ExpectedRevision: 1, Actor: optimization.IdentityRef{Subject: "reviewer"}, Reason: "source evidence withdrawn"}
		abort := errors.New("abort revoke")
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if err := tx.RevokeLesson(revoke); err != nil {
				return err
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		lesson, err := s.GetLesson(ctx, revoke.LessonID)
		if err != nil || !lesson.RevokedAt.IsZero() || lesson.Revision != 1 {
			t.Fatal("aborted revocation changed eligibility")
		}
		if _, err = s.GetPublication(ctx, revoke.LessonID, 2); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatal("aborted revocation left outbox")
		}
		for range 2 {
			if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RevokeLesson(revoke) }); err != nil {
				t.Fatal(err)
			}
		}
		lesson, err = s.GetLesson(ctx, revoke.LessonID)
		if err != nil || lesson.Revision != 2 || lesson.RevokedAt.IsZero() || lesson.RevocationReason != revoke.Reason {
			t.Fatalf("revocation lost: %+v %v", lesson, err)
		}
		old, err := s.GetPublication(ctx, lesson.ID, 1)
		if err != nil || old.SupersededAt.IsZero() || !old.Payload.RevokedAt.IsZero() {
			t.Fatal("old publication payload changed or stayed eligible")
		}
		publication, err := s.GetPublication(ctx, lesson.ID, 2)
		if err != nil || publication.Operation != optimization.PublicationRevoke || publication.Payload.RevocationReason != revoke.Reason || publication.RemoteAggregateID != request.RemoteAggregateID || publication.PayloadDigest == old.PayloadDigest {
			t.Fatalf("revocation outbox lost: %+v %v", publication, err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.PutPublication(request) }); !errors.Is(err, optimization.ErrStaleVersion) {
			t.Fatalf("late publish resurrected lesson: %v", err)
		}
		revoke.Reason = "changed reason"
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RevokeLesson(revoke) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("revocation overwritten: %v", err)
		}
		events, err := s.ListEvents(ctx, request.ExperimentID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		count := 0
		for _, event := range events.Items {
			if event.Type == "lesson_revoked" {
				count++
				if !slices.Contains(event.Resources, lesson.ID) {
					t.Fatal("revocation omitted lesson identity")
				}
			}
		}
		if count != 1 {
			t.Fatalf("revocation events=%d", count)
		}
	})
	t.Run("LessonBindingsFreezeAndRevocationPausesConsumer", func(t *testing.T) {
		s, ctx, request := publishedLessonFixture(t, factory)
		consumer := Fixture(t, "candidate")
		consumer.Experiment.Spec.Scope = request.Lesson.Scope
		// Reuse a compatible version bundle already bound to the exact scope.
		producer, err := s.GetExperiment(ctx, request.ExperimentID)
		if err != nil {
			t.Fatal(err)
		}
		consumer.Experiment.Spec = producer.Spec
		consumer.Experiment.Spec.LessonCutoff = time.Now().Add(time.Second)
		consumer.Experiment.EvaluationPlan = producer.EvaluationPlan
		consumer.Versions = nil
		consumer.Experiment.LessonIDs = []id.ID{request.Lesson.ID}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(consumer) }); err != nil {
			t.Fatal(err)
		}
		frozen, err := s.GetExperiment(ctx, consumer.Experiment.ID)
		if err != nil || len(frozen.FrozenLessons) != 1 || frozen.FrozenLessons[0].Revision != 1 || len(frozen.FrozenLessons[0].Digest) != 64 {
			t.Fatalf("lesson binding not frozen: %+v %v", frozen.FrozenLessons, err)
		}
		if frozen.SpecDigest == producer.SpecDigest {
			t.Fatal("lesson set omitted from frozen specification")
		}
		queue := optimization.Transition{ExperimentID: frozen.ID, ExpectedRevision: frozen.Revision, NextState: optimization.StateQueued, IdempotencyKey: "start-consumer", Actor: producer.Spec.Requester}
		commitTrialChange(ctx, t, s, queue)
		var lease optimization.WorkLease
		if err = s.Transact(ctx, func(tx store.Tx) error {
			var claimErr error
			lease, claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: frozen.ID, Owner: "consumer", Duration: time.Minute})
			return claimErr
		}); err != nil {
			t.Fatal(err)
		}
		revoke := optimization.LessonRevocation{LessonID: request.Lesson.ID, ExpectedRevision: 1, Actor: optimization.IdentityRef{Subject: "reviewer"}, Reason: "invalid source"}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RevokeLesson(revoke) }); err != nil {
			t.Fatal(err)
		}
		paused, err := s.GetExperiment(ctx, frozen.ID)
		if err != nil || paused.State != optimization.StatePaused || paused.Failure != "lesson_revoked" || paused.FrozenLessons[0].Digest != frozen.FrozenLessons[0].Digest {
			t.Fatalf("consumer not invalidated: %+v %v", paused, err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RenewLease(lease) }); err == nil {
			t.Fatal("revoked consumer renewed work")
		}
		consumer.Experiment.ID = id.New(optimization.PrefixExperiment)
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(consumer) }); !errors.Is(err, optimization.ErrStaleVersion) {
			t.Fatalf("new consumer recalled revoked lesson: %v", err)
		}
	})
}

func publicationRequest(bundle optimization.ExperimentBundle, lease optimization.WorkLease, decision optimization.Decision) optimization.PublicationRequest {
	lessonID := id.New(optimization.PrefixLesson)
	return optimization.PublicationRequest{ExperimentID: bundle.Experiment.ID, Lease: lease, Actor: bundle.Experiment.Spec.Requester, RemoteAggregateID: lessonID.String(), Lesson: optimization.Lesson{ID: lessonID, Scope: bundle.Experiment.Spec.Scope, Domain: "reasoning", HarnessSchema: 1, Statement: "Check source evidence before answering.", Applicability: []string{"source-based questions"}, DecisionIDs: []id.ID{decision.ID}, EvidenceClass: optimization.EvidenceExploratory}}
}

func publishedLessonFixture(t *testing.T, factory Factory) (store.Store, context.Context, optimization.PublicationRequest) {
	t.Helper()
	s, ctx, bundle, lease, decision := decisionFixture(t, factory)
	commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
	request := publicationRequest(bundle, lease, decision)
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.PutPublication(request) }); err != nil {
		t.Fatal(err)
	}
	return s, ctx, request
}
