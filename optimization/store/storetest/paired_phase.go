package storetest

import (
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func pairedPhase(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("PairedValidationWaitsForWholeRoster", func(t *testing.T) {
		s := factory(t, time.Now)
		parent := Fixture(t, "paired-phase")
		ctx := scoped(parent.Experiment.Spec.Scope)
		parentLease := validationExperiment(ctx, t, s, parent)
		children := make([]optimization.ExperimentBundle, 2)
		leases := make([]optimization.WorkLease, 2)
		request := allocationRequest(parent, parentLease)
		request.PairedRoster = nil
		for index := range children {
			children[index] = parent
			children[index].Experiment.ID = id.New(optimization.PrefixExperiment)
			children[index].Versions = nil
			leases[index] = validationExperiment(ctx, t, s, children[index])
			request.PairedRoster = append(request.PairedRoster, allocationRequest(children[index], leases[index]).PairedRoster[0])
		}
		allocation := allocate(ctx, t, s, request)
		access := optimization.ValidationAccess{AllocationID: allocation.ID, ExperimentID: children[0].Experiment.ID, Lease: leases[0]}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.ExposeValidation(access) }); err != nil {
			t.Fatal(err)
		}
		fillVersionTrials(ctx, t, s, children[0], leases[0], children[0].Experiment.Spec.Baseline, id.ID{}, optimization.SplitValidation, allocation.ID, nil)
		assertPhaseRefused(ctx, t, s, children[0], leases[0], optimization.PhaseDecide)
		assertPhaseRefused(ctx, t, s, parent, parentLease, optimization.PhaseDecide)
		fillVersionTrials(ctx, t, s, children[1], leases[1], children[1].Experiment.Spec.Baseline, id.ID{}, optimization.SplitValidation, allocation.ID, nil)
		advanceFixture(ctx, t, s, children[0], leases[0], optimization.PhaseDecide)
		advanceFixture(ctx, t, s, children[1], leases[1], optimization.PhaseDecide)
		advanceFixture(ctx, t, s, parent, parentLease, optimization.PhaseDecide)
	})
	t.Run("ValidationFinalistMustBelongToItsExperiment", func(t *testing.T) {
		s := factory(t, time.Now)
		parent := candidateBundle(t)
		ctx := scoped(parent.Experiment.Spec.Scope)
		lease := validationExperiment(ctx, t, s, parent)
		child := parent
		child.Experiment.ID = id.New(optimization.PrefixExperiment)
		childLease := proposalExperiment(ctx, t, s, child)
		candidate, version := candidateVersion(t, child, "Check evidence before answering.")
		change := candidateTransition(ctx, t, s, child, childLease, candidate, version)
		commitTrialChange(ctx, t, s, change)
		request := allocationRequest(parent, lease)
		request.PairedRoster[0].Finalist = candidate.Version
		if err := s.Transact(ctx, func(tx store.Tx) error { _, err := tx.AllocateValidation(request); return err }); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("borrowed foreign finalist: %v", err)
		}
	})
}
