package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

type recalledLessons struct {
	page  optimization.LessonPage
	err   error
	after func()
	calls int
}

func (r *recalledLessons) Recall(context.Context, optimization.RecallQuery) (optimization.LessonPage, error) {
	r.calls++
	if r.after != nil {
		r.after()
	}
	return r.page, r.err
}
func (*recalledLessons) Publish(context.Context, optimization.Lesson) (optimization.PublicationReceipt, error) {
	return optimization.PublicationReceipt{}, optimization.ErrUnavailable
}
func (*recalledLessons) Revoke(context.Context, id.ID, int64) error {
	return optimization.ErrUnavailable
}

type unusedPhase struct{}

func (unusedPhase) Execute(context.Context, optimization.WorkItem) (optimization.Transition, error) {
	return optimization.Transition{}, optimization.ErrUnavailable
}

func lessonCreation(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("CreateFreezesRecalledLessonEvidence", func(t *testing.T) {
		s, ctx, publication := publishedLessonFixture(t, factory)
		lesson, err := s.GetLesson(ctx, publication.Lesson.ID)
		if err != nil {
			t.Fatal(err)
		}
		source := &recalledLessons{page: optimization.LessonPage{Items: []optimization.Lesson{lesson}}}
		authority := &publicationAuthority{actor: publication.Actor}
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), Lessons: source})
		if err != nil {
			t.Fatal(err)
		}
		bundle := candidateBundle(t)
		bundle.Experiment.Spec.LessonCutoff = time.Now().UTC()
		query := optimization.RecallQuery{Scope: bundle.Experiment.Spec.Scope, Domain: lesson.Domain, HarnessSchema: 1, Query: "source evidence", Cutoff: bundle.Experiment.Spec.LessonCutoff, Limit: 10}
		created, err := op.CreateWithLessons(ctx, bundle, query)
		if err != nil {
			t.Fatal(err)
		}
		digest, err := optimization.LessonDigest(lesson)
		if err != nil {
			t.Fatal(err)
		}
		if len(created.FrozenLessons) != 1 || created.FrozenLessons[0] != (optimization.LessonRef{ID: lesson.ID, Revision: 1, Digest: digest}) {
			t.Fatalf("unfrozen recall: %+v", created.FrozenLessons)
		}
		source.page.Items[0].Statement = "changed remote answer"
		stored, err := s.GetExperiment(ctx, created.ID)
		if err != nil || stored.SpecDigest != created.SpecDigest || stored.FrozenLessons[0].Digest != digest {
			t.Fatal("source mutation changed experiment")
		}
		if len(bundle.Experiment.LessonIDs) != 0 || source.calls != 1 {
			t.Fatal("creation mutated the caller or repeated recall")
		}
	})
	for _, scenario := range []string{"scope", "cutoff", "content", "domain", "duplicate", "partial", "outage", "revoked", "authority"} {
		t.Run("CreateRejectsIncompleteRecall/"+scenario, func(t *testing.T) {
			s, ctx, publication := publishedLessonFixture(t, factory)
			lesson, err := s.GetLesson(ctx, publication.Lesson.ID)
			if err != nil {
				t.Fatal(err)
			}
			source := &recalledLessons{page: optimization.LessonPage{Items: []optimization.Lesson{lesson}}}
			authority := &publicationAuthority{actor: publication.Actor}
			op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig(), Lessons: source})
			if err != nil {
				t.Fatal(err)
			}
			bundle := candidateBundle(t)
			bundle.Experiment.Spec.LessonCutoff = time.Now().UTC()
			query := optimization.RecallQuery{Scope: bundle.Experiment.Spec.Scope, Domain: lesson.Domain, HarnessSchema: 1, Query: "source evidence", Cutoff: bundle.Experiment.Spec.LessonCutoff, Limit: 10}
			switch scenario {
			case "scope":
				query.Scope = optimizationScope("foreign")
			case "cutoff":
				query.Cutoff = query.Cutoff.Add(time.Second)
			case "content":
				source.page.Items[0].Statement = "forged reflection"
			case "domain":
				source.page.Items[0].Domain = "foreign domain"
			case "duplicate":
				source.page.Items = append(source.page.Items, lesson)
			case "partial":
				source.page.NextCursor = "more"
			case "outage":
				source.err = optimization.ErrUnavailable
			case "revoked":
				source.after = func() {
					if revokeErr := s.Transact(ctx, func(tx store.Tx) error {
						return tx.RevokeLesson(optimization.LessonRevocation{LessonID: lesson.ID, ExpectedRevision: 1, Actor: publication.Actor, Reason: "withdrawn during recall"})
					}); revokeErr != nil {
						t.Fatal(revokeErr)
					}
				}
			case "authority":
				source.after = func() { authority.denied = true }
			}
			if _, err = op.CreateWithLessons(ctx, bundle, query); err == nil {
				t.Fatal("invalid recall created experiment")
			}
			if _, err = s.GetExperiment(ctx, bundle.Experiment.ID); !errors.Is(err, optimization.ErrNotFound) {
				t.Fatalf("failed creation persisted: %v", err)
			}
			if (scenario == "scope" || scenario == "cutoff") && source.calls != 0 {
				t.Fatal("invalid request reached remote recall")
			}
		})
	}
	t.Run("CreationChecksExpectedLessonDigestAtomically", func(t *testing.T) {
		s, ctx, publication := publishedLessonFixture(t, factory)
		bundle := candidateBundle(t)
		bundle.Experiment.Spec.LessonCutoff = time.Now().UTC()
		bundle.Experiment.LessonIDs = []id.ID{publication.Lesson.ID}
		bundle.ExpectedLessons = []optimization.LessonRef{{ID: publication.Lesson.ID, Revision: 1, Digest: "wrong"}}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("changed lesson accepted: %v", err)
		}
	})
}
