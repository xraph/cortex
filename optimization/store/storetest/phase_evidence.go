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

func phaseEvidence(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("BaselineWaitsForEveryRepetitionAndScore", func(t *testing.T) {
		s, ctx, bundle, lease := trialFixture(t, factory)
		assertPhaseRefused(ctx, t, s, bundle, lease, optimization.PhasePropose)
		var last optimization.Evaluation
		for repetition := range bundle.Experiment.Spec.Limits.Repetitions {
			trial := queuedTrial(bundle, 0)
			trial.Repetition = repetition
			value := captureFixtureTrial(ctx, t, s, bundle, lease, trial)
			if repetition < bundle.Experiment.Spec.Limits.Repetitions-1 {
				persistFixtureEvaluation(ctx, t, s, bundle, lease, value)
				assertPhaseRefused(ctx, t, s, bundle, lease, optimization.PhasePropose)
			} else {
				last = value
			}
		}
		assertPhaseRefused(ctx, t, s, bundle, lease, optimization.PhasePropose)
		change := trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "last-score-and-propose", nil, nil)
		change.NextPhase = optimization.PhasePropose
		change.Evaluations = []optimization.Evaluation{last}
		commitTrialChange(ctx, t, s, change)
		current, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil || current.Round != 1 || current.Phase != optimization.PhasePropose {
			t.Fatal("complete baseline did not advance")
		}
	})
	t.Run("ProbeWaitsForAcceptedCandidates", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := candidateBundle(t)
		ctx := scoped(bundle.Experiment.Spec.Scope)
		lease := proposalExperiment(ctx, t, s, bundle)
		assertPhaseRefused(ctx, t, s, bundle, lease, optimization.PhaseProbe)
		candidate, version := candidateVersion(t, bundle, "Check the evidence before answering.")
		change := candidateTransition(ctx, t, s, bundle, lease, candidate, version)
		current, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		change.ExpectedRevision = current.Revision
		commitTrialChange(ctx, t, s, change)
		assertPhaseRefused(ctx, t, s, bundle, lease, optimization.PhaseCompose)
		assertPhaseRefused(ctx, t, s, bundle, lease, optimization.PhaseValidate)
		fillVersionTrials(ctx, t, s, bundle, lease, candidate.Version, candidate.ID, optimization.SplitDiscovery, id.ID{}, nil)
		advanceFixture(ctx, t, s, bundle, lease, optimization.PhaseValidate)
	})
	t.Run("ValidationWaitsForAllocatedArms", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := candidateBundle(t)
		ctx := scoped(bundle.Experiment.Spec.Scope)
		lease := proposalExperiment(ctx, t, s, bundle)
		candidate, version := candidateVersion(t, bundle, "Check the evidence before answering.")
		change := candidateTransition(ctx, t, s, bundle, lease, candidate, version)
		current, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		change.ExpectedRevision = current.Revision
		commitTrialChange(ctx, t, s, change)
		fillVersionTrials(ctx, t, s, bundle, lease, candidate.Version, candidate.ID, optimization.SplitDiscovery, id.ID{}, nil)
		advanceFixture(ctx, t, s, bundle, lease, optimization.PhaseValidate)
		assertPhaseRefused(ctx, t, s, bundle, lease, optimization.PhaseDecide)
		request := allocationRequest(bundle, lease)
		request.PairedRoster[0].Finalist = candidate.Version
		allocation := allocate(ctx, t, s, request)
		if err = s.Transact(ctx, func(tx store.Tx) error {
			return tx.ExposeValidation(optimization.ValidationAccess{AllocationID: allocation.ID, ExperimentID: bundle.Experiment.ID, Lease: lease})
		}); err != nil {
			t.Fatal(err)
		}
		fillVersionTrials(ctx, t, s, bundle, lease, bundle.Experiment.Spec.Baseline, id.ID{}, optimization.SplitValidation, allocation.ID, nil)
		assertPhaseRefused(ctx, t, s, bundle, lease, optimization.PhaseDecide)
		fillVersionTrials(ctx, t, s, bundle, lease, candidate.Version, candidate.ID, optimization.SplitValidation, allocation.ID, nil)
		advanceFixture(ctx, t, s, bundle, lease, optimization.PhaseDecide)
	})
}

func assertPhaseRefused(ctx context.Context, t *testing.T, s store.Store, bundle optimization.ExperimentBundle, lease optimization.WorkLease, next optimization.Phase) {
	t.Helper()
	before, err := s.GetExperiment(ctx, bundle.Experiment.ID)
	if err != nil {
		t.Fatal(err)
	}
	change := trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "premature-"+string(next), nil, nil)
	change.NextPhase = next
	if err = s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrIncompleteEvidence) {
		t.Fatalf("phase %s advanced without evidence: %v", next, err)
	}
	after, err := s.GetExperiment(ctx, bundle.Experiment.ID)
	if err != nil || after.Phase != before.Phase || after.Revision != before.Revision {
		t.Fatal("refused phase changed state")
	}
}

func advanceFixture(ctx context.Context, t *testing.T, s store.Store, bundle optimization.ExperimentBundle, lease optimization.WorkLease, next optimization.Phase) {
	t.Helper()
	change := trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "advance-"+string(next), nil, nil)
	change.NextPhase = next
	commitTrialChange(ctx, t, s, change)
}

func fillVersionTrials(ctx context.Context, t *testing.T, s store.Store, bundle optimization.ExperimentBundle, lease optimization.WorkLease, version optimization.VersionRef, candidate id.ID, split optimization.Split, allocation id.ID, existing []optimization.Trial, artifacts ...optimization.ArtifactRef) {
	t.Helper()
	for taskIndex, task := range bundle.Experiment.Spec.Tasks {
		if task.Split != split {
			continue
		}
		for repetition := range bundle.Experiment.Spec.Limits.Repetitions {
			skip := false
			for _, prior := range existing {
				if prior.Version == version && prior.CaseDigest == task.Digest && prior.Repetition == repetition {
					skip = true
				}
			}
			if skip {
				continue
			}
			trial := queuedTrial(bundle, taskIndex)
			trial.Repetition = repetition
			trial.Version = version
			trial.CandidateID = candidate
			trial.AllocationID = allocation
			value := captureFixtureTrial(ctx, t, s, bundle, lease, trial, artifacts...)
			persistFixtureEvaluation(ctx, t, s, bundle, lease, value)
		}
	}
}

func captureFixtureTrial(ctx context.Context, t *testing.T, s store.Store, bundle optimization.ExperimentBundle, lease optimization.WorkLease, trial optimization.Trial, artifacts ...optimization.ArtifactRef) optimization.Evaluation {
	t.Helper()
	receipt := openTrial(ctx, t, s, bundle, lease, trial)
	version, err := s.GetVersion(ctx, trial.Version.ID)
	if err != nil {
		t.Fatal(err)
	}
	captureBundle := bundle
	captureBundle.Versions = []optimization.HarnessVersion{version}
	record := capturedExecution(t, captureBundle, receipt)
	record.Artifacts = artifacts
	for _, task := range bundle.Experiment.Spec.Tasks {
		if task.ID == trial.CaseID {
			record.InputDigest = task.InputDigest
		}
	}
	record, err = optimization.SealExecution(record)
	if err != nil {
		t.Fatal(err)
	}
	scenario := trialScenario{store: s, ctx: ctx, bundle: bundle, lease: lease, trial: trial, receipt: receipt}
	update := scenario.update(t, optimization.TrialCompleted)
	update.Execution = &record
	commitTrialChange(ctx, t, s, scenario.change(t, "capture/"+trial.ID.String(), update))
	value := evaluationFor(scenario, record)
	value.ID = "job/" + trial.ID.String()
	value.ReportID = "report/" + trial.ID.String()
	return value
}

func persistFixtureEvaluation(ctx context.Context, t *testing.T, s store.Store, bundle optimization.ExperimentBundle, lease optimization.WorkLease, value optimization.Evaluation) {
	t.Helper()
	change := trialTransition(ctx, t, s, bundle.Experiment.ID, lease, "evaluation/"+value.ID, nil, nil)
	change.Evaluations = []optimization.Evaluation{value}
	commitTrialChange(ctx, t, s, change)
}
