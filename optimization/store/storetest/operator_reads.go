package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/id"

	"github.com/xraph/cortex/optimization"
)

func operatorReads(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("OperatorReadsBindParentScopeAndCurrentAuthority", func(t *testing.T) {
		s, ctx, bundle, lease, decision := decisionFixture(t, factory)
		commitTrialChange(ctx, t, s, decisionChange(ctx, t, s, bundle, lease, decision))
		publication := publicationRequest(bundle, lease, decision)
		if err := s.Transact(ctx, func(tx optimization.Transaction) error { return tx.PutPublication(publication) }); err != nil {
			t.Fatal(err)
		}
		authority := &publicationAuthority{actor: bundle.Experiment.Spec.Requester}
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig()})
		if err != nil {
			t.Fatal(err)
		}
		candidates, err := op.ListCandidates(ctx, bundle.Experiment.ID, optimization.PageQuery{Limit: 1})
		if err != nil || len(candidates.Items) != 1 || !candidates.Complete {
			t.Fatalf("candidates: %+v %v", candidates, err)
		}
		candidate := candidates.Items[0]
		trials, err := op.ListTrials(ctx, bundle.Experiment.ID, optimization.TrialQuery{PageQuery: optimization.PageQuery{Limit: 1}})
		if err != nil || len(trials.Items) != 1 || trials.Complete || trials.NextCursor == "" {
			t.Fatalf("trials: %+v %v", trials, err)
		}
		trial := trials.Items[0]
		checks := map[string]func(context.Context) error{
			"candidates": func(c context.Context) error {
				_, e := op.ListCandidates(c, bundle.Experiment.ID, optimization.PageQuery{})
				return e
			},
			"candidate": func(c context.Context) error {
				_, e := op.GetCandidate(c, bundle.Experiment.ID, candidate.ID)
				return e
			},
			"trials": func(c context.Context) error {
				_, e := op.ListTrials(c, bundle.Experiment.ID, optimization.TrialQuery{})
				return e
			},
			"trial": func(c context.Context) error { _, e := op.GetTrial(c, bundle.Experiment.ID, trial.ID); return e },
			"comparison": func(c context.Context) error {
				_, e := op.GetComparison(c, bundle.Experiment.ID, optimization.PageQuery{Limit: 1})
				return e
			},
			"versions": func(c context.Context) error {
				_, e := op.ListVersions(c, optimization.VersionQuery{AgentID: bundle.Experiment.Spec.AgentID})
				return e
			},
			"version": func(c context.Context) error { _, e := op.GetVersion(c, candidate.Version.ID); return e },
			"events": func(c context.Context) error {
				_, e := op.ListEvents(c, bundle.Experiment.ID, optimization.PageQuery{Limit: 1})
				return e
			},
			"lessons": func(c context.Context) error { _, e := op.ListLessons(c, optimization.LessonQuery{}); return e },
			"publications": func(c context.Context) error {
				_, e := op.ListPublications(c, optimization.PublicationQuery{ExperimentID: bundle.Experiment.ID})
				return e
			},
			"activations": func(c context.Context) error {
				_, e := op.ListActivations(c, optimization.ActivationQuery{AgentID: bundle.Experiment.Spec.AgentID})
				return e
			},
		}
		checks["lesson"] = func(c context.Context) error { _, e := op.GetLesson(c, publication.Lesson.ID); return e }

		for name, read := range checks {
			t.Run(name, func(t *testing.T) {
				if readErr := read(ctx); readErr != nil {
					t.Fatal(readErr)
				}
				authority.denied = true
				if readErr := read(ctx); !errors.Is(readErr, optimization.ErrPolicyDenied) {
					t.Fatalf("denied read: %v", readErr)
				}
				authority.denied = false
				if readErr := read(context.Background()); readErr == nil {
					t.Fatal("unscoped read succeeded")
				}
			})
		}
		other := Fixture(t, "candidate")
		if err = s.Transact(ctx, func(tx optimization.Transaction) error { return tx.CreateExperiment(other) }); err != nil {
			t.Fatal(err)
		}
		if _, err = op.GetCandidate(ctx, other.Experiment.ID, candidate.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("candidate accepted wrong parent: %v", err)
		}
		if _, err = op.GetTrial(ctx, other.Experiment.ID, trial.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("trial accepted wrong parent: %v", err)
		}
		foreign := scoped(Fixture(t, "foreign-operator").Experiment.Spec.Scope)
		if _, err = op.GetVersion(foreign, candidate.Version.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("foreign version leaked: %v", err)
		}
		for _, limit := range []int{-1, 101} {
			if _, err = op.ListCandidates(ctx, bundle.Experiment.ID, optimization.PageQuery{Limit: limit}); !errors.Is(err, optimization.ErrInvalidSpec) {
				t.Fatalf("invalid limit accepted: %v", err)
			}
		}
		comparison, err := op.GetComparison(ctx, bundle.Experiment.ID, optimization.PageQuery{Limit: 1})
		if err != nil || comparison.Decision == nil || comparison.Decision.ID != decision.ID || comparison.Evaluations.Complete {
			t.Fatalf("comparison: %+v %v", comparison, err)
		}
		page, err := op.ListEvents(ctx, bundle.Experiment.ID, optimization.PageQuery{Limit: 1})
		if err != nil || page.Complete || page.NextCursor == "" {
			t.Fatalf("event continuation: %+v %v", page, err)
		}
		next, err := op.ListEvents(ctx, bundle.Experiment.ID, optimization.PageQuery{Limit: 1, Cursor: page.NextCursor})
		if err != nil || len(next.Items) != 1 || next.Items[0].Sequence <= page.Items[0].Sequence {
			t.Fatalf("event replay cursor: %+v %v", next, err)
		}
		if _, err = op.ListEvents(ctx, other.Experiment.ID, optimization.PageQuery{Cursor: page.NextCursor}); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("cross-experiment event cursor accepted: %v", err)
		}
		for _, cursor := range []string{"-1", "01", "x", "9223372036854775808"} {
			if _, err = op.ListEvents(ctx, bundle.Experiment.ID, optimization.PageQuery{Cursor: cursor}); !errors.Is(err, optimization.ErrInvalidSpec) {
				t.Fatalf("invalid event cursor accepted: %q %v", cursor, err)
			}
		}
	})
}

type operatorReadAuthority struct {
	*publicationAuthority
	deniedID id.ID
}

func (a *operatorReadAuthority) Authorize(ctx context.Context, action optimization.Action, resource optimization.Resource) error {
	if !a.deniedID.IsNil() && resource.ID == a.deniedID {
		return optimization.ErrPolicyDenied
	}
	return a.publicationAuthority.Authorize(ctx, action, resource)
}

func operatorReadPages(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("OperatorPagesBoundDefaultsAndRefuseDeniedItems", func(t *testing.T) {
		s := factory(t, time.Now)
		bundles := make([]optimization.ExperimentBundle, 26)
		for i := range bundles {
			bundles[i] = Fixture(t, "operator-pages")
		}
		ctx := scoped(bundles[0].Experiment.Spec.Scope)
		if err := s.Transact(ctx, func(tx optimization.Transaction) error {
			for _, bundle := range bundles {
				if err := tx.CreateExperiment(bundle); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		authority := &operatorReadAuthority{publicationAuthority: &publicationAuthority{actor: bundles[0].Experiment.Spec.Requester}}
		op, err := optimization.NewOptimizer(optimization.OptimizerConfig{Repository: s, Authorizer: authority, IdentityResolver: authority, Caller: authority, Handler: unusedPhase{}, Worker: optimization.DefaultWorkerConfig()})
		if err != nil {
			t.Fatal(err)
		}
		first, err := op.ListExperiments(ctx, optimization.ListQuery{})
		if err != nil || len(first.Items) != 25 || first.Complete || first.NextCursor == "" {
			t.Fatalf("default page: %+v %v", first, err)
		}
		last, err := op.ListExperiments(ctx, optimization.ListQuery{Cursor: first.NextCursor})
		if err != nil || len(last.Items) != 1 || !last.Complete || last.NextCursor != "" {
			t.Fatalf("final page: %+v %v", last, err)
		}
		authority.deniedID = first.Items[0].ID
		if page, readErr := op.ListExperiments(ctx, optimization.ListQuery{}); !errors.Is(readErr, optimization.ErrPolicyDenied) || len(page.Items) != 0 {
			t.Fatalf("unauthorized page leaked: %+v %v", page, readErr)
		}
		if page, readErr := op.List(ctx, optimization.ListQuery{}); !errors.Is(readErr, optimization.ErrPolicyDenied) || len(page.Items) != 0 {
			t.Fatalf("legacy list bypassed grants: %+v %v", page, readErr)
		}
		authority.deniedID = bundles[0].Versions[0].ID
		if page, readErr := op.ListVersions(ctx, optimization.VersionQuery{AgentID: bundles[0].Experiment.Spec.AgentID}); !errors.Is(readErr, optimization.ErrPolicyDenied) || len(page.Items) != 0 {
			t.Fatalf("unauthorized version leaked: %+v %v", page, readErr)
		}
		authority.deniedID = id.Nil
		empty, err := op.ListExperiments(scoped(Fixture(t, "empty-operator").Experiment.Spec.Scope), optimization.ListQuery{})
		if err != nil || empty.Items == nil || len(empty.Items) != 0 || !empty.Complete {
			t.Fatalf("empty scope misrepresented: %+v %v", empty, err)
		}
	})
}
