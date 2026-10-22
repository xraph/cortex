package storetest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func artifacts(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("MissingArtifactBlocksDecisionAndDeletionInvalidatesActivation", func(t *testing.T) {
		now := time.Now().UTC()
		fixed := func(t *testing.T, _ func() time.Time) store.Store { return factory(t, func() time.Time { return now }) }
		refs := func(bundle optimization.ExperimentBundle) []optimization.ArtifactRef {
			return []optimization.ArtifactRef{artifactRegistration(bundle, optimization.WorkLease{}, now).Ref}
		}
		s, ctx, bundle, lease, decision := decisionFixtureWithArtifacts(t, fixed, nil, refs, func(bundle *optimization.ExperimentBundle) {
			bundle.Experiment.Spec.Objective.MinValidationCases = 1
			bundle.Experiment.Spec.Objective.RequireReproducibility = false
		})
		inconclusive := decision
		inconclusive.Reasons = []string{"required trace unavailable"}
		inconclusive.Comparison.MissingReasons = []string{"required trace unavailable"}
		preview := errors.New("rollback inconclusive preview")
		previewChange := decisionChange(ctx, t, s, bundle, lease, inconclusive)
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if decisionErr := tx.CommitTransition(previewChange); decisionErr != nil {
				return decisionErr
			}
			return preview
		}); !errors.Is(err, preview) {
			t.Fatalf("missing evidence could not be recorded as inconclusive: %v", err)
		}
		decision.Outcome, decision.EvidenceClass, decision.Comparison.Complete, decision.Comparison.MissingReasons = optimization.DecisionRecommend, optimization.EvidenceValidated, true, nil
		change := decisionChange(ctx, t, s, bundle, lease, decision)
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("missing artifact recommended: %v", err)
		}
		registration := artifactRegistration(bundle, lease, now)
		registerArtifact(ctx, t, s, registration)
		commitTrialChange(ctx, t, s, change)
		request := activationRequest(bundle, decision)
		active := activate(ctx, t, s, request)
		publication := publicationRequest(bundle, lease, decision)
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.PutPublication(publication) }); err != nil {
			t.Fatal(err)
		}
		lesson, err := s.GetLesson(ctx, publication.Lesson.ID)
		if err != nil {
			t.Fatal(err)
		}
		outbox, err := s.GetPublication(ctx, lesson.ID, lesson.Revision)
		if err != nil {
			t.Fatal(err)
		}
		lessonRef := optimization.LessonRef{ID: lesson.ID, Revision: lesson.Revision, Digest: outbox.PayloadDigest}
		consumer := Fixture(t, "artifact-consumer")
		producer, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		consumer.Experiment.Spec, consumer.Experiment.EvaluationPlan = producer.Spec, producer.EvaluationPlan
		consumer.Experiment.Spec.LessonCutoff = now.Add(time.Second)
		consumer.Experiment.LessonIDs = []id.ID{lesson.ID}
		consumer.Versions = nil
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(consumer) }); err != nil {
			t.Fatal(err)
		}
		unrelated := registration
		unrelated.Ref.Kind = "unreferenced-debug"
		registerArtifact(ctx, t, s, unrelated)
		unrelatedDeletion := optimization.ArtifactDeletion{Ref: unrelated.Ref, RequestID: "delete-unreferenced", Actor: registration.Actor, Reason: "cleanup"}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.BeginArtifactDeletion(unrelatedDeletion) }); err != nil {
			t.Fatal(err)
		}
		if _, err = s.GetActivation(ctx, request.AgentID, request.Channel); err != nil {
			t.Fatalf("unrelated deletion invalidated activation: %v", err)
		}
		if _, err = s.ResolveLesson(ctx, lessonRef); err != nil {
			t.Fatalf("unrelated deletion revoked lesson: %v", err)
		}
		now = registration.ExpiresAt
		if _, err = s.GetActivation(ctx, request.AgentID, request.Channel); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("expired evidence remained active: %v", err)
		}
		if _, err = s.ResolveLesson(ctx, lessonRef); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("expired evidence remained recallable: %v", err)
		}
		now = registration.CreatedAt.Add(time.Minute)
		deletion := optimization.ArtifactDeletion{Ref: registration.Ref, RequestID: "delete-evidence", Actor: registration.Actor, Reason: "source removed"}
		abort := errors.New("abort")
		if err = s.Transact(ctx, func(tx store.Tx) error {
			if deleteErr := tx.BeginArtifactDeletion(deletion); deleteErr != nil {
				return deleteErr
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if _, err = s.GetActivation(ctx, request.AgentID, request.Channel); err != nil {
			t.Fatalf("aborted deletion invalidated activation: %v", err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.BeginArtifactDeletion(deletion) }); err != nil {
			t.Fatal(err)
		}
		if _, err = s.GetActivation(ctx, request.AgentID, request.Channel); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("deleting evidence active: %v", err)
		}
		if history, historyErr := s.GetActivationGeneration(ctx, active.ID); historyErr != nil || history.ID != active.ID {
			t.Fatalf("lost audit history: %v", historyErr)
		}
		request.ID, request.ExpectedGeneration, request.RollbackTo = id.New(optimization.PrefixActivation), 1, active.ID
		if err = s.Transact(ctx, func(tx store.Tx) error { _, activateErr := tx.CompareAndSwapActivation(request); return activateErr }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("deleted evidence rolled back: %v", err)
		}
		lesson, err = s.GetLesson(ctx, lesson.ID)
		if err != nil || lesson.RevokedAt.IsZero() || lesson.Revision != 2 {
			t.Fatalf("dependent lesson not revoked: %+v %v", lesson, err)
		}
		outbox, err = s.GetPublication(ctx, lesson.ID, 2)
		if err != nil || outbox.Operation != optimization.PublicationRevoke {
			t.Fatalf("remote revocation not queued: %+v %v", outbox, err)
		}
		paused, err := s.GetExperiment(ctx, consumer.Experiment.ID)
		if err != nil || paused.State != optimization.StatePaused || paused.Failure != "lesson_revoked" {
			t.Fatalf("lesson consumer not paused: %+v %v", paused, err)
		}
	})
	t.Run("ArtifactRegistrationAndDeletionAtomic", func(t *testing.T) {
		now := time.Now().UTC()
		s := factory(t, func() time.Time { return now })
		bundle := Fixture(t, "artifact-retention")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		lease := validationExperiment(ctx, t, s, bundle)
		request := artifactRegistration(bundle, lease, now)
		for _, mode := range []string{"lease", "actor", "future", "expired", "experiment"} {
			invalid := request
			switch mode {
			case "lease":
				invalid.Lease.Generation++
			case "actor":
				invalid.Actor.Subject = " "
			case "future":
				invalid.CreatedAt = now.Add(time.Second)
			case "expired":
				invalid.ExpiresAt = now
			case "experiment":
				invalid.Ref.ExperimentID = id.New(optimization.PrefixExperiment)
			}
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.RegisterArtifact(invalid) }); err == nil {
				t.Fatalf("accepted invalid %s registration", mode)
			}
		}
		registerArtifact(ctx, t, s, request)
		registerArtifact(ctx, t, s, request)
		got, err := s.GetArtifact(ctx, request.Ref)
		if err != nil || got.Revision != 1 || !got.RegisteredAt.Equal(now) {
			t.Fatalf("registration: %+v %v", got, err)
		}
		got.Ref.Scope.Levels[0].Value = "mutated"
		if _, err = s.GetArtifact(scoped(Fixture(t, "foreign-artifact").Experiment.Spec.Scope), request.Ref); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("foreign read: %v", err)
		}
		changed := request
		changed.ExpiresAt = changed.ExpiresAt.Add(time.Hour)
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RegisterArtifact(changed) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("retention rewritten: %v", err)
		}
		deletion := optimization.ArtifactDeletion{Ref: request.Ref, RequestID: "privacy-request", Actor: bundle.Experiment.Spec.Requester, Reason: "source removed"}
		abort := errors.New("abort deletion")
		if err = s.Transact(ctx, func(tx store.Tx) error {
			if deleteErr := tx.BeginArtifactDeletion(deletion); deleteErr != nil {
				return deleteErr
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		got, err = s.GetArtifact(ctx, request.Ref)
		if err != nil || got.Revision != 1 || got.Deletion != nil {
			t.Fatal("aborted deletion escaped")
		}
		for range 2 {
			if err = s.Transact(ctx, func(tx store.Tx) error { return tx.BeginArtifactDeletion(deletion) }); err != nil {
				t.Fatal(err)
			}
		}
		got, err = s.GetArtifact(ctx, request.Ref)
		if err != nil || got.Revision != 2 || got.Deletion == nil || !got.DeletedAt.IsZero() {
			t.Fatalf("pending deletion: %+v %v", got, err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RegisterArtifact(request) }); !errors.Is(err, optimization.ErrStaleVersion) {
			t.Fatalf("resurrected: %v", err)
		}
		changedDeletion := deletion
		changedDeletion.Reason = "different reason"
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.BeginArtifactDeletion(changedDeletion) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatal(err)
		}
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CompleteArtifactDeletion(request.Ref, "wrong-request") }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatal(err)
		}
		for range 2 {
			if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CompleteArtifactDeletion(request.Ref, deletion.RequestID) }); err != nil {
				t.Fatal(err)
			}
		}
		got, err = s.GetArtifact(ctx, request.Ref)
		if err != nil || got.Revision != 3 || !got.DeletedAt.Equal(now) {
			t.Fatalf("tombstone: %+v %v", got, err)
		}
		events, err := s.ListEvents(ctx, bundle.Experiment.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		counts := map[string]int{}
		for _, event := range events.Items {
			counts[event.Type]++
		}
		for _, kind := range []string{"artifact_registered", "artifact_deletion_requested", "artifact_deleted"} {
			if counts[kind] != 1 {
				t.Fatalf("%s events=%d", kind, counts[kind])
			}
		}
	})
}

func artifactRegistration(bundle optimization.ExperimentBundle, lease optimization.WorkLease, now time.Time) optimization.ArtifactRegistration {
	return optimization.ArtifactRegistration{Ref: optimization.ArtifactRef{ExperimentID: bundle.Experiment.ID, Scope: bundle.Experiment.Spec.Scope, Digest: strings.Repeat("a", 64), Size: 10, MediaType: "text/plain", Kind: "trace", AccessPolicy: "restricted"}, CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Actor: bundle.Experiment.Spec.Requester, Lease: lease}
}

func registerArtifact(ctx context.Context, t *testing.T, s store.Store, request optimization.ArtifactRegistration) {
	t.Helper()
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.RegisterArtifact(request) }); err != nil {
		t.Fatal(err)
	}
}
