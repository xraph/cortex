package storetest

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

// DiscoveryInheritance checks arm isolation and frozen earlier-task learning.
func DiscoveryInheritance(t *testing.T, factory Factory) {
	t.Helper()
	discoveryController(t, factory)
	t.Run("LaterTaskReadsOnlyEarlierLessons", func(t *testing.T) {
		f := discoveryStream(t, factory, 2)
		child := f.children[2]
		child.ParentLease = &f.leases[0]
		if err := f.s.Transact(f.ctx, func(tx store.Tx) error { return tx.CreateExperiment(child) }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("missing earlier reflection accepted: %v", err)
		}
		request := discoveryRequest(t, f, 0)
		var source optimization.DiscoveryLessons
		if err := f.s.Transact(f.ctx, func(tx store.Tx) error { var err error; source, err = tx.PutDiscoveryLessons(request); return err }); err != nil {
			t.Fatal(err)
		}
		if err := f.s.Transact(f.ctx, func(tx store.Tx) error { return tx.CreateExperiment(child) }); err != nil {
			t.Fatal(err)
		}
		saved, err := f.s.GetExperiment(f.ctx, child.Experiment.ID)
		expected := optimization.DiscoveryRef{ExperimentID: request.ExperimentID, Digest: source.Digest}
		if err != nil || len(saved.Discoveries) != 1 || saved.Discoveries[0] != expected {
			t.Fatalf("earlier source not frozen: %+v %v", saved.Discoveries, err)
		}
		resolved, err := f.s.ResolveDiscoveryLessons(f.ctx, saved.ID, expected)
		if err != nil || !reflect.DeepEqual(resolved, source) {
			t.Fatalf("own earlier lesson unavailable: %v", err)
		}
		for _, target := range []id.ID{f.parent.Experiment.ID, f.children[0].Experiment.ID, f.children[1].Experiment.ID} {
			if _, err = f.s.ResolveDiscoveryLessons(f.ctx, target, expected); !errors.Is(err, optimization.ErrPolicyDenied) {
				t.Fatalf("source exposed to unrelated consumer: %v", err)
			}
		}
		changed := expected
		changed.Digest = "changed"
		if _, err = f.s.ResolveDiscoveryLessons(f.ctx, saved.ID, changed); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("changed reference accepted: %v", err)
		}
		other := f.children[3]
		other.ParentLease = &f.leases[0]
		if err = f.s.Transact(f.ctx, func(tx store.Tx) error { return tx.CreateExperiment(other) }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("other arm borrowed reflection: %v", err)
		}
	})
	t.Run("DiscoveryEvidenceDeletionBlocksHeldDispatch", func(t *testing.T) {
		var registered optimization.ArtifactRegistration
		f := discoveryStream(t, factory, 2, func(s store.Store, child optimization.ExperimentBundle, lease optimization.WorkLease) []optimization.ArtifactRef {
			if child.Experiment.Spec.Parent.Arm != 0 {
				return nil
			}
			registered = artifactRegistration(child, lease, time.Now())
			registerArtifact(scoped(child.Experiment.Spec.Scope), t, s, registered)
			return []optimization.ArtifactRef{registered.Ref}
		})
		request := discoveryRequest(t, f, 0)
		if err := f.s.Transact(f.ctx, func(tx store.Tx) error { _, err := tx.PutDiscoveryLessons(request); return err }); err != nil {
			t.Fatal(err)
		}
		child := f.children[2]
		child.ParentLease = &f.leases[0]
		lease := proposalExperiment(f.ctx, t, f.s, child)
		saved, err := f.s.GetExperiment(f.ctx, child.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		held := reservation(saved.ID, lease, "held-before-delete", 1)
		reserve(f.ctx, t, f.s, held)
		if err = f.s.Transact(f.ctx, func(tx store.Tx) error {
			return tx.BeginArtifactDeletion(optimization.ArtifactDeletion{Ref: registered.Ref, RequestID: "delete-source", Actor: registered.Actor, Reason: "removed source"})
		}); err != nil {
			t.Fatal(err)
		}
		if _, err = f.s.ResolveDiscoveryLessons(f.ctx, saved.ID, saved.Discoveries[0]); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("deleted evidence remained usable: %v", err)
		}
		if err = beginUsage(f.ctx, f.s, held, strings.Repeat("a", 64)); !errors.Is(err, optimization.ErrIncompleteEvidence) {
			t.Fatalf("deleted source admitted held dispatch: %v", err)
		}
		retained, err := f.s.GetReservation(f.ctx, saved.ID, held.CallKey)
		if err != nil || retained.Call != nil || retained.DebitedTokens != 1 {
			t.Fatalf("refused dispatch changed reservation: %+v %v", retained, err)
		}
	})
	t.Run("DiscoveryReferencesCannotBeSuppliedByCaller", func(t *testing.T) {
		f := discoveryStream(t, factory, 2)
		child := f.children[2]
		child.ParentLease = &f.leases[0]
		child.Experiment.Discoveries = []optimization.DiscoveryRef{{ExperimentID: f.children[1].Experiment.ID, Digest: "caller"}}
		if err := f.s.Transact(f.ctx, func(tx store.Tx) error { return tx.CreateExperiment(child) }); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("caller-selected learning accepted: %v", err)
		}
	})
}

// DiscoveryInheritanceRecovery retains the exact earlier-task references across reopen.
func DiscoveryInheritanceRecovery(t *testing.T, open func(*testing.T, func() time.Time) store.Store) {
	t.Helper()
	now := time.Now().UTC()
	f := discoveryStream(t, func(t *testing.T, _ func() time.Time) store.Store { return open(t, func() time.Time { return now }) }, 2)
	request := discoveryRequest(t, f, 0)
	var source optimization.DiscoveryLessons
	if err := f.s.Transact(f.ctx, func(tx store.Tx) error { var err error; source, err = tx.PutDiscoveryLessons(request); return err }); err != nil {
		t.Fatal(err)
	}
	child := f.children[2]
	child.ParentLease = &f.leases[0]
	oldLease := proposalExperiment(f.ctx, t, f.s, child)
	before, err := f.s.GetExperiment(f.ctx, child.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.s.Close(); err != nil {
		t.Fatal(err)
	}
	now = f.leases[0].ExpiresAt.Add(time.Second)
	f.s = open(t, func() time.Time { return now })
	after, err := f.s.GetExperiment(f.ctx, child.Experiment.ID)
	if err != nil || !reflect.DeepEqual(after.Discoveries, before.Discoveries) || after.SpecDigest != before.SpecDigest {
		t.Fatalf("reopen changed inheritance: %v", err)
	}
	resolved, err := f.s.ResolveDiscoveryLessons(f.ctx, after.ID, after.Discoveries[0])
	if err != nil || !reflect.DeepEqual(resolved, source) {
		t.Fatalf("reopened earlier lesson changed: %v", err)
	}
	var lease optimization.WorkLease
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error {
		if _, claimErr := tx.ClaimWork(optimization.ClaimRequest{ExperimentID: f.parent.Experiment.ID, Owner: "recovered-parent", Duration: time.Minute}); claimErr != nil {
			return claimErr
		}
		var claimErr error
		lease, claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: after.ID, Owner: "recovered-child", Duration: time.Minute})
		return claimErr
	}); err != nil {
		t.Fatal(err)
	}
	if err = f.s.Transact(f.ctx, func(tx store.Tx) error {
		_, reserveErr := tx.Reserve(reservation(after.ID, oldLease, "stale", 1))
		return reserveErr
	}); !errors.Is(err, optimization.ErrConflict) {
		t.Fatalf("old inherited worker accepted: %v", err)
	}
	reserve(f.ctx, t, f.s, reservation(after.ID, lease, "resumed", 1))
}
