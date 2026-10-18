package storetest

import (
	"errors"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func candidateEvidence(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("CandidateReferencesOwnedDiscoveryEvidence", func(t *testing.T) {
		s, value := evaluatedTrial(t, factory)
		commitTrialChange(s.ctx, t, s.store, evaluationChange(t, s, value))
		advance := trialTransition(s.ctx, t, s.store, s.bundle.Experiment.ID, s.lease, "propose-with-evidence", nil, nil)
		advance.NextPhase = optimization.PhasePropose
		commitTrialChange(s.ctx, t, s.store, advance)
		candidate := optimization.Candidate{ID: id.New(optimization.PrefixCandidate), ExperimentID: s.bundle.Experiment.ID, Origin: optimization.OriginGeneral, Round: 1, Proposal: optimization.Proposal{Rationale: "No permitted change addresses this failure", EvidenceIDs: []id.ID{s.trial.ID}}, Rejection: "no eligible edit"}
		change := trialTransition(s.ctx, t, s.store, s.bundle.Experiment.ID, s.lease, "reject-with-evidence", nil, nil)
		change.NextPhase = optimization.PhaseProbe
		change.Candidates = []optimization.Candidate{candidate}
		commitTrialChange(s.ctx, t, s.store, change)
		stored, err := s.store.GetCandidate(s.ctx, candidate.ID)
		if err != nil || len(stored.Proposal.EvidenceIDs) != 1 || stored.Proposal.EvidenceIDs[0] != s.trial.ID {
			t.Fatalf("lost discovery evidence: %+v %v", stored, err)
		}
	})
	t.Run("CandidateCannotBorrowAnotherExperimentsEvidence", func(t *testing.T) {
		s, value := evaluatedTrial(t, factory)
		commitTrialChange(s.ctx, t, s.store, evaluationChange(t, s, value))
		other := s.bundle
		other.Experiment.ID = id.New(optimization.PrefixExperiment)
		lease := proposalExperiment(s.ctx, t, s.store, other)
		candidate := optimization.Candidate{ID: id.New(optimization.PrefixCandidate), ExperimentID: other.Experiment.ID, Origin: optimization.OriginGeneral, Round: 1, Proposal: optimization.Proposal{EvidenceIDs: []id.ID{s.trial.ID}}, Rejection: "foreign evidence"}
		change := trialTransition(s.ctx, t, s.store, other.Experiment.ID, lease, "foreign-evidence", nil, nil)
		change.NextPhase = optimization.PhaseProbe
		change.Candidates = []optimization.Candidate{candidate}
		if err := s.store.Transact(s.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("borrowed foreign experiment evidence: %v", err)
		}
	})
	t.Run("CandidateRejectsDuplicateOrUnscoredEvidence", func(t *testing.T) {
		for _, evaluated := range []bool{false, true} {
			s, value := evaluatedTrial(t, factory)
			if evaluated {
				commitTrialChange(s.ctx, t, s.store, evaluationChange(t, s, value))
			}
			advance := trialTransition(s.ctx, t, s.store, s.bundle.Experiment.ID, s.lease, "propose", nil, nil)
			advance.NextPhase = optimization.PhasePropose
			commitTrialChange(s.ctx, t, s.store, advance)
			evidence := []id.ID{s.trial.ID}
			if evaluated {
				evidence = append(evidence, s.trial.ID)
			}
			candidate := optimization.Candidate{ID: id.New(optimization.PrefixCandidate), ExperimentID: s.bundle.Experiment.ID, Origin: optimization.OriginGeneral, Round: 1, Proposal: optimization.Proposal{EvidenceIDs: evidence}, Rejection: "bad evidence"}
			change := trialTransition(s.ctx, t, s.store, s.bundle.Experiment.ID, s.lease, "bad-evidence", nil, nil)
			change.NextPhase = optimization.PhaseProbe
			change.Candidates = []optimization.Candidate{candidate}
			if err := s.store.Transact(s.ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err == nil {
				t.Fatal("duplicate or unscored evidence admitted")
			}
		}
	})
}
