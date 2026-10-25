package storetest

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func validation(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ValidationAllocationCannotBeReused", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "holdout")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		first := validationExperiment(ctx, t, s, bundle)
		request := allocationRequest(bundle, first)
		original := allocate(ctx, t, s, request)
		again := allocate(ctx, t, s, request)
		if original.ID != again.ID || original.Revision != again.Revision {
			t.Fatal("identical allocation retry changed identity")
		}
		if err := s.Transact(ctx, func(tx store.Tx) error {
			return tx.ExposeValidation(optimization.ValidationAccess{AllocationID: original.ID, ExperimentID: bundle.Experiment.ID, Lease: first})
		}); err != nil {
			t.Fatal(err)
		}
		other := bundle
		other.Experiment.ID = id.New(optimization.PrefixExperiment)
		other.Experiment.Spec.Tasks = append([]optimization.TaskCase(nil), bundle.Experiment.Spec.Tasks...)
		renamed := other.Experiment.Spec.Tasks[1]
		renamed.ID = "renamed-case"
		renamed.Input = "\r\n " + renamed.Input + " \r\n"
		var sealErr error
		other.Experiment.Spec.Tasks[1], sealErr = optimization.SealTaskCase(renamed)
		if sealErr != nil {
			t.Fatal(sealErr)
		}
		if other.Experiment.Spec.Tasks[1].InputDigest != bundle.Experiment.Spec.Tasks[1].InputDigest || other.Experiment.Spec.Tasks[1].Digest == bundle.Experiment.Spec.Tasks[1].Digest {
			t.Fatal("renamed fixture must share only normalized input identity")
		}
		renamedVersion := bundle.Versions[0]
		renamedVersion.ID = id.New(optimization.PrefixVersion)
		renamedVersion.HarnessID = id.New(optimization.PrefixHarness)
		renamedVersion.Name = "New harness name"
		other.Versions = []optimization.HarnessVersion{renamedVersion}
		other.Experiment.Spec.Baseline = optimization.VersionRef{ID: renamedVersion.ID, Digest: renamedVersion.Digest}
		other.Experiment.Spec.Evaluation.Digest = strings.Repeat("c", 64)
		other.Experiment.EvaluationPlan = fixtureEvaluationPlan(other.Experiment.Spec)
		second := validationExperiment(ctx, t, s, other)
		if err := s.Transact(ctx, func(tx store.Tx) error { _, err := tx.AllocateValidation(allocationRequest(other, second)); return err }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("exposed inputs reused by another experiment: %v", err)
		}
		changed := request
		changed.PairedRoster = append([]optimization.ValidationArm(nil), request.PairedRoster...)
		changed.PairedRoster[0].Finalist.Digest = "different"
		if err := s.Transact(ctx, func(tx store.Tx) error { _, err := tx.AllocateValidation(changed); return err }); err == nil {
			t.Fatal("frozen finalist replaced")
		}
		if err := s.Transact(ctx, func(tx store.Tx) error {
			return tx.ReleaseValidation(optimization.ValidationAccess{AllocationID: original.ID, ExperimentID: bundle.Experiment.ID, Lease: first})
		}); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("exposed allocation released: %v", err)
		}
		stored, err := s.GetValidationAllocation(ctx, original.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.ExposedAt.IsZero() || len(stored.InputDigests) != 1 || stored.InputDigests[0] != bundle.Experiment.Spec.Tasks[1].InputDigest {
			t.Fatalf("allocation lost frozen input identity: %+v", stored)
		}
	})
	t.Run("ValidationAllocationReleaseAndScope", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "release-holdout")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		lease := validationExperiment(ctx, t, s, bundle)
		request := allocationRequest(bundle, lease)
		original := allocate(ctx, t, s, request)
		foreign := scoped(Fixture(t, "foreign-holdout").Experiment.Spec.Scope)
		if _, err := s.GetValidationAllocation(foreign, original.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("allocation escaped exact scope: %v", err)
		}
		access := optimization.ValidationAccess{AllocationID: original.ID, ExperimentID: bundle.Experiment.ID, Lease: lease}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.ReleaseValidation(access) }); err != nil {
			t.Fatal(err)
		}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.ExposeValidation(access) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("released allocation exposed: %v", err)
		}
		request.ID = id.New(optimization.PrefixValidation)
		replacement := allocate(ctx, t, s, request)
		if replacement.ID == original.ID {
			t.Fatal("released allocation identity recycled")
		}
	})
	t.Run("ValidationPairedRoster", func(t *testing.T) {
		f := frozenValidationFamily(t, factory, 1)
		s, ctx, parent, request := f.s, f.ctx, f.parent, f.request
		arms, leases := request.PairedRoster, f.leases[1:]
		allocation := allocate(ctx, t, s, request)
		for index, arm := range arms {
			access := optimization.ValidationAccess{AllocationID: allocation.ID, ExperimentID: arm.ExperimentID, Lease: leases[index]}
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.ExposeValidation(access) }); err != nil {
				t.Fatal(err)
			}
		}
		outsider := parent
		outsider.Experiment.Spec.Improvement = nil
		outsider.Experiment.ID = id.New(optimization.PrefixExperiment)
		outsider.Versions = nil
		outsiderLease := validationExperiment(ctx, t, s, outsider)
		if err := s.Transact(ctx, func(tx store.Tx) error {
			return tx.ExposeValidation(optimization.ValidationAccess{AllocationID: allocation.ID, ExperimentID: outsider.Experiment.ID, Lease: outsiderLease})
		}); !errors.Is(err, optimization.ErrPolicyDenied) {
			t.Fatalf("unrelated child consumed paired cases: %v", err)
		}
		changed := request
		changed.PairedRoster = append([]optimization.ValidationArm(nil), arms...)
		changed.PairedRoster[1].ExperimentID = outsider.Experiment.ID
		if err := s.Transact(ctx, func(tx store.Tx) error { _, err := tx.AllocateValidation(changed); return err }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("paired roster replaced after exposure: %v", err)
		}
	})
	t.Run("ValidationAllocationRaceAndRollback", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := Fixture(t, "race-holdout")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		lease := validationExperiment(ctx, t, s, bundle)
		requests := []optimization.ValidationAllocationRequest{allocationRequest(bundle, lease), allocationRequest(bundle, lease)}
		var group sync.WaitGroup
		results := make(chan error, 2)
		for _, request := range requests {
			group.Add(1)
			go func() {
				defer group.Done()
				results <- s.Transact(ctx, func(tx store.Tx) error { _, err := tx.AllocateValidation(request); return err })
			}()
		}
		group.Wait()
		close(results)
		wins, conflicts := 0, 0
		for err := range results {
			switch {
			case err == nil:
				wins++
			case errors.Is(err, optimization.ErrConflict):
				conflicts++
			default:
				t.Fatal(err)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("same holdout allocated twice: %d/%d", wins, conflicts)
		}
		var winner optimization.ValidationAllocation
		for _, request := range requests {
			if value, err := s.GetValidationAllocation(ctx, request.ID); err == nil {
				winner = value
			} else if !errors.Is(err, optimization.ErrNotFound) {
				t.Fatal(err)
			}
		}
		before, err := s.ListEvents(ctx, bundle.Experiment.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		access := optimization.ValidationAccess{AllocationID: winner.ID, ExperimentID: bundle.Experiment.ID, Lease: lease}
		abort := errors.New("abort exposure")
		if err = s.Transact(ctx, func(tx store.Tx) error {
			if exposeErr := tx.ExposeValidation(access); exposeErr != nil {
				return exposeErr
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		stored, err := s.GetValidationAllocation(ctx, winner.ID)
		if err != nil {
			t.Fatal(err)
		}
		after, err := s.ListEvents(ctx, bundle.Experiment.ID, 0, 100)
		if err != nil {
			t.Fatal(err)
		}
		if !stored.ExposedAt.IsZero() || stored.Revision != 1 || len(before.Items) != len(after.Items) {
			t.Fatal("exposure or event escaped rollback")
		}
		stored.PairedRoster[0].Finalist.Digest = "mutated"
		stored.InputDigests[0] = "mutated"
		fresh, err := s.GetValidationAllocation(ctx, winner.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.PairedRoster[0].Finalist.Digest != bundle.Experiment.Spec.Baseline.Digest || fresh.InputDigests[0] != bundle.Experiment.Spec.Tasks[1].InputDigest {
			t.Fatal("allocation read shared backing memory")
		}
		access.Lease.Generation++
		if err = s.Transact(ctx, func(tx store.Tx) error { return tx.ExposeValidation(access) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("stale worker exposed validation: %v", err)
		}
	})
	t.Run("ValidationPairedEvaluationMustMatch", func(t *testing.T) {
		s := factory(t, time.Now)
		parent, _ := improvementFixture(t)
		child := &parent.Experiment.Spec.Improvement.Children[1]
		child.Spec.Evaluation.Revision = "different-scorer"
		child.EvaluationPlan = fixtureEvaluationPlan(child.Spec)
		if err := s.Transact(scoped(parent.Experiment.Spec.Scope), func(tx store.Tx) error { return tx.CreateExperiment(parent) }); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("paired arms used different evaluation bindings: %v", err)
		}
	})
}

func validationExperiment(ctx context.Context, t *testing.T, s store.Store, bundle optimization.ExperimentBundle, setup ...func(optimization.WorkLease) []optimization.ArtifactRef) optimization.WorkLease {
	t.Helper()
	lease := proposalExperiment(ctx, t, s, bundle, setup...)
	change := trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "no-change-proposal", nil, nil)
	change.NextPhase = optimization.PhaseProbe
	change.Candidates = []optimization.Candidate{{ID: id.New(optimization.PrefixCandidate), ExperimentID: bundle.Experiment.ID, Origin: optimization.OriginGeneral, Round: 1, Rejection: "No candidate improves this fixture"}}
	commitTrialChange(ctx, t, s, change)
	advanceFixture(ctx, t, s, bundle, lease, optimization.PhaseValidate)
	return lease
}

func allocationRequest(bundle optimization.ExperimentBundle, lease optimization.WorkLease) optimization.ValidationAllocationRequest {
	return optimization.ValidationAllocationRequest{ID: id.New(optimization.PrefixValidation), ExperimentID: bundle.Experiment.ID, Lease: lease, PairedRoster: []optimization.ValidationArm{{ExperimentID: bundle.Experiment.ID, Controller: bundle.Experiment.Spec.Controller, Baseline: bundle.Experiment.Spec.Baseline, Finalist: bundle.Experiment.Spec.Baseline}}}
}

func allocate(ctx context.Context, t *testing.T, s store.Store, request optimization.ValidationAllocationRequest) optimization.ValidationAllocation {
	t.Helper()
	var allocation optimization.ValidationAllocation
	if err := s.Transact(ctx, func(tx store.Tx) error { var err error; allocation, err = tx.AllocateValidation(request); return err }); err != nil {
		t.Fatal(err)
	}
	return allocation
}
