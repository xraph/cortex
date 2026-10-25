package storetest

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func discoveryFamily(t *testing.T, factory Factory) validationFamily {
	t.Helper()
	return discoveryStream(t, factory, 1)
}

func discoveryStream(t *testing.T, factory Factory, pairs int, setup ...func(store.Store, optimization.ExperimentBundle, optimization.WorkLease) []optimization.ArtifactRef) validationFamily {
	t.Helper()
	parent, children := validationFamilyBundles(t, pairs)
	f := validationFamily{s: factory(t, time.Now), ctx: scoped(parent.Experiment.Spec.Scope), parent: parent, children: children, leases: make([]optimization.WorkLease, len(children)+1)}
	if err := f.s.Transact(f.ctx, func(tx store.Tx) error {
		var err error
		f.leases[0], err = startFixture(tx, parent, "parent")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		children[i].ParentLease = &f.leases[0]
		f.leases[i+1] = validationExperiment(f.ctx, t, f.s, children[i], func(lease optimization.WorkLease) []optimization.ArtifactRef {
			refs := make([]optimization.ArtifactRef, 0, len(setup))
			for _, prepare := range setup {
				refs = append(refs, prepare(f.s, children[i], lease)...)
			}
			return refs
		})
	}
	return f
}

func discoveryRequest(t *testing.T, f validationFamily, arm int) optimization.DiscoveryLessonsRequest {
	t.Helper()
	child := f.children[arm]
	trials, err := f.s.ListTrials(f.ctx, child.Experiment.ID, optimization.TrialQuery{PageQuery: optimization.PageQuery{Limit: 100}, Split: optimization.SplitDiscovery})
	if err != nil || len(trials.Items) == 0 {
		t.Fatalf("discovery fixture: %v", err)
	}
	return optimization.DiscoveryLessonsRequest{ExperimentID: child.Experiment.ID, Lease: f.leases[arm+1], Domain: "comparison", Lessons: []optimization.LessonDraft{{Domain: "comparison", Statement: "Check the captured outcome.", EvidenceIDs: []id.ID{trials.Items[0].ID}}}}
}

// DiscoveryLearning checks provisional reflection persistence before holdout exposure.
func DiscoveryLearning(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("EmptyDiscoveryReflectionIsDurable", func(t *testing.T) {
		f := discoveryFamily(t, factory)
		request := discoveryRequest(t, f, 0)
		request.Lessons = []optimization.LessonDraft{}
		if err := f.s.Transact(f.ctx, func(tx store.Tx) error { _, err := tx.PutDiscoveryLessons(request); return err }); err != nil {
			t.Fatal(err)
		}
		stored, err := f.s.GetDiscoveryLessons(f.ctx, request.ExperimentID)
		if err != nil || stored.Lessons == nil || len(stored.Lessons) != 0 {
			t.Fatalf("empty result lost: %v", err)
		}
	})
	t.Run("DiscoveryReflectionRollsBackWithAudit", func(t *testing.T) {
		f := discoveryFamily(t, factory)
		request := discoveryRequest(t, f, 0)
		abort := errors.New("abort reflection")
		err := f.s.Transact(f.ctx, func(tx store.Tx) error {
			if _, putErr := tx.PutDiscoveryLessons(request); putErr != nil {
				return putErr
			}
			return abort
		})
		if !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if _, err = f.s.GetDiscoveryLessons(f.ctx, request.ExperimentID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("aborted reflection persisted: %v", err)
		}
		events, err := f.s.ListEvents(f.ctx, request.ExperimentID, 0, 500)
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range events.Items {
			if event.Type == "discovery_lessons_recorded" {
				t.Fatal("aborted reflection left audit")
			}
		}
	})
	t.Run("DiscoveryLessonsBindSourceAndRemainPrivate", func(t *testing.T) {
		f := discoveryFamily(t, factory)
		req := discoveryRequest(t, f, 0)
		var recorded optimization.DiscoveryLessons
		if err := f.s.Transact(f.ctx, func(tx store.Tx) error { var err error; recorded, err = tx.PutDiscoveryLessons(req); return err }); err != nil {
			t.Fatal(err)
		}
		if recorded.ExperimentID != req.ExperimentID || recorded.ParentExperimentID != f.parent.Experiment.ID || recorded.Controller != f.children[0].Experiment.Spec.Controller || recorded.Arm != 0 || recorded.Sequence != 0 || recorded.CreatedAt.IsZero() || recorded.Digest == "" {
			t.Fatalf("unbound discovery reflection: %+v", recorded)
		}
		if err := f.s.Transact(f.ctx, func(tx store.Tx) error {
			again, err := tx.PutDiscoveryLessons(req)
			if err == nil && !reflect.DeepEqual(again, recorded) {
				t.Fatal("retry changed reflection")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		req.Lessons[0].Statement = "changed"
		if err := f.s.Transact(f.ctx, func(tx store.Tx) error { _, err := tx.PutDiscoveryLessons(req); return err }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("replaced reflection: %v", err)
		}
		stored, err := f.s.GetDiscoveryLessons(f.ctx, req.ExperimentID)
		if err != nil || !reflect.DeepEqual(stored, recorded) {
			t.Fatalf("reflection changed: %v", err)
		}
		foreign := scoped(Fixture(t, "foreign-discovery").Experiment.Spec.Scope)
		if _, err = f.s.GetDiscoveryLessons(foreign, req.ExperimentID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("cross-scope reflection: %v", err)
		}
		lessons, err := f.s.ListLessons(f.ctx, optimization.LessonQuery{PageQuery: optimization.PageQuery{Limit: 100}})
		if err != nil || len(lessons.Items) != 0 {
			t.Fatalf("provisional lesson published: %v", err)
		}
	})
	for _, mode := range []string{"foreign-evidence", "missing-evidence", "stale-lease", "parent-stopped", "validation-started", "nil-result"} {
		t.Run("DiscoveryLessonsReject/"+mode, func(t *testing.T) {
			f := discoveryFamily(t, factory)
			req := discoveryRequest(t, f, 0)
			want := optimization.ErrPolicyDenied
			switch mode {
			case "foreign-evidence":
				req.Lessons[0].EvidenceIDs = discoveryRequest(t, f, 1).Lessons[0].EvidenceIDs
			case "missing-evidence":
				req.Lessons[0].EvidenceIDs = []id.ID{id.New(optimization.PrefixTrial)}
			case "parent-stopped":
				current, err := f.s.GetExperiment(f.ctx, f.parent.Experiment.ID)
				if err != nil {
					t.Fatal(err)
				}
				if err = f.s.Transact(f.ctx, func(tx store.Tx) error {
					return tx.CommitTransition(optimization.Transition{ExperimentID: current.ID, ExpectedRevision: current.Revision, NextState: optimization.StatePaused, IdempotencyKey: "pause", Actor: current.Spec.Requester})
				}); err != nil {
					t.Fatal(err)
				}
				want = optimization.ErrConflict
			case "stale-lease":
				req.Lease.Generation++
				want = optimization.ErrConflict
			case "validation-started":
				for i := range f.children {
					initial := discoveryRequest(t, f, i)
					initial.Lessons = []optimization.LessonDraft{}
					if err := f.s.Transact(f.ctx, func(tx store.Tx) error { _, putErr := tx.PutDiscoveryLessons(initial); return putErr }); err != nil {
						t.Fatal(err)
					}
				}
				advanceFixture(f.ctx, t, f.s, f.parent, f.leases[0], optimization.PhaseValidate)
				want = optimization.ErrConflict
			case "nil-result":
				req.Lessons = nil
				want = optimization.ErrInvalidSpec
			}
			err := f.s.Transact(f.ctx, func(tx store.Tx) error { _, err := tx.PutDiscoveryLessons(req); return err })
			if !errors.Is(err, want) {
				t.Fatalf("%s accepted: %v", mode, err)
			}
		})
	}
}

// DiscoveryLearningRecovery verifies that reflection acknowledgement survives
// restart and lease takeover without rewriting content or duplicating its audit.
func DiscoveryLearningRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	now := time.Now().UTC()
	f := discoveryFamily(t, func(t *testing.T, _ func() time.Time) store.Store { return open(t, func() time.Time { return now }) })
	request := discoveryRequest(t, f, 0)
	var original optimization.DiscoveryLessons
	if err := f.s.Transact(f.ctx, func(tx store.Tx) error { var err error; original, err = tx.PutDiscoveryLessons(request); return err }); err != nil {
		t.Fatal(err)
	}
	if err := f.s.Close(); err != nil {
		t.Fatal(err)
	}
	now = f.leases[0].ExpiresAt.Add(time.Second)
	f.s = open(t, func() time.Time { return now })
	saved, err := f.s.GetDiscoveryLessons(f.ctx, request.ExperimentID)
	if err != nil || !reflect.DeepEqual(saved, original) {
		t.Fatalf("reopened reflection: %+v %v", saved, err)
	}
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error {
		var claimErr error
		f.leases[0], claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: f.parent.Experiment.ID, Owner: "recovered-parent", Duration: time.Minute})
		if claimErr != nil {
			return claimErr
		}
		f.leases[1], claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: request.ExperimentID, Owner: "recovered-child", Duration: time.Minute})
		return claimErr
	}); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error { _, putErr := tx.PutDiscoveryLessons(request); return putErr }); !errors.Is(err, optimization.ErrConflict) {
		t.Fatalf("old worker accepted: %v", err)
	}
	request.Lease = f.leases[1]
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error {
		retried, putErr := tx.PutDiscoveryLessons(request)
		if putErr == nil && !reflect.DeepEqual(retried, original) {
			t.Fatal("recovery rewrote reflection")
		}
		return putErr
	}); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	f.s = open(t, func() time.Time { return now })
	saved, err = f.s.GetDiscoveryLessons(f.ctx, request.ExperimentID)
	if err != nil || !reflect.DeepEqual(saved, original) {
		t.Fatalf("second reopen changed reflection: %v", err)
	}
	events, err := f.s.ListEvents(f.ctx, request.ExperimentID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	recorded := 0
	for _, event := range events.Items {
		if event.Type == "discovery_lessons_recorded" {
			recorded++
		}
	}
	if recorded != 1 {
		t.Fatalf("reflection audit duplicated: %d", recorded)
	}
}
