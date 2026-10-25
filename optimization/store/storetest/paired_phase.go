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
		f := frozenValidationFamily(t, factory, 1)
		s, ctx, parent, children := f.s, f.ctx, f.parent, f.children
		parentLease, leases, request := f.leases[0], f.leases[1:], f.request
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
