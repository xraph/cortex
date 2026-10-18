package storetest

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

func candidates(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("CandidateVersionAndPhaseAtomic", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := candidateBundle(t)
		ctx := scoped(bundle.Experiment.Spec.Scope)
		lease := proposalExperiment(ctx, t, s, bundle)
		candidate, version := candidateVersion(t, bundle, "Check the evidence before answering.")
		change := candidateTransition(bundle, lease, candidate, version)
		abort := errors.New("abort candidate")
		if err := s.Transact(ctx, func(tx store.Tx) error {
			if err := tx.CommitTransition(change); err != nil {
				return err
			}
			return abort
		}); !errors.Is(err, abort) {
			t.Fatal(err)
		}
		if _, err := s.GetCandidate(ctx, candidate.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("candidate escaped rollback: %v", err)
		}
		if _, err := s.GetVersion(ctx, version.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("candidate version escaped rollback: %v", err)
		}
		for range 2 {
			if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err != nil {
				t.Fatal(err)
			}
		}
		stored, err := s.GetCandidate(ctx, candidate.ID)
		if err != nil {
			t.Fatal(err)
		}
		experiment, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Version != candidate.Version || stored.CreatedAt.IsZero() || experiment.Phase != optimization.PhaseProbe || experiment.Revision != 5 {
			t.Fatalf("candidate/phase not committed together: %+v %+v", stored, experiment)
		}
		stored.Proposal.Edits[0].Text = "tampered"
		fresh, err := s.GetCandidate(ctx, candidate.ID)
		if err != nil {
			t.Fatal(err)
		}
		if fresh.Proposal.Edits[0].Text != candidate.Proposal.Edits[0].Text {
			t.Fatal("candidate read shares backing memory")
		}
		foreign := scoped(Fixture(t, "foreign-candidate").Experiment.Spec.Scope)
		if _, err = s.GetCandidate(foreign, candidate.ID); !errors.Is(err, optimization.ErrNotFound) {
			t.Fatalf("candidate escaped scope: %v", err)
		}
	})
	t.Run("CandidateCannotInventFrozenVersion", func(t *testing.T) {
		for _, mode := range []string{"wrong-experiment", "wrong-version", "policy-change", "invalid-origin", "unrecorded-parent", "unused-version"} {
			t.Run(mode, func(t *testing.T) {
				s := factory(t, time.Now)
				bundle := candidateBundle(t)
				ctx := scoped(bundle.Experiment.Spec.Scope)
				lease := proposalExperiment(ctx, t, s, bundle)
				candidate, version := candidateVersion(t, bundle, "Check the evidence before answering.")
				change := candidateTransition(bundle, lease, candidate, version)
				switch mode {
				case "wrong-experiment":
					change.Candidates[0].ExperimentID = id.New(optimization.PrefixExperiment)
				case "wrong-version":
					change.Candidates[0].Proposal.Edits[0].Text = "Different instructions."
				case "policy-change":
					altered := version
					altered.Policy.AllowToolSubset = true
					var err error
					altered, err = optimization.SealVersion(altered)
					if err != nil {
						t.Fatal(err)
					}
					change.Versions[0] = altered
					change.Candidates[0].Version.Digest = altered.Digest
				case "invalid-origin":
					change.Candidates[0].Origin = "forged"
				case "unrecorded-parent":
					change.Candidates[0].Parents = []id.ID{id.New(optimization.PrefixCandidate)}
				case "unused-version":
					extra := version
					extra.ID = id.New(optimization.PrefixVersion)
					change.Versions = append(change.Versions, extra)
				}
				if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err == nil {
					t.Fatal("invalid candidate resources committed")
				}
				stored, err := s.GetExperiment(ctx, bundle.Experiment.ID)
				if err != nil {
					t.Fatal(err)
				}
				if stored.Phase != optimization.PhasePropose || stored.Revision != 4 {
					t.Fatal("rejected candidate advanced phase")
				}
			})
		}
	})
	t.Run("CandidateDigestUniqueAndRejectedRetained", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := candidateBundle(t)
		ctx := scoped(bundle.Experiment.Spec.Scope)
		lease := proposalExperiment(ctx, t, s, bundle)
		candidate, version := candidateVersion(t, bundle, "Check the evidence before answering.")
		duplicate := candidate
		duplicate.ID = id.New(optimization.PrefixCandidate)
		change := candidateTransition(bundle, lease, candidate, version)
		change.Candidates = append(change.Candidates, duplicate)
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("duplicate candidate content admitted: %v", err)
		}
		rejected := optimization.Candidate{ID: id.New(optimization.PrefixCandidate), ExperimentID: bundle.Experiment.ID, Origin: optimization.OriginDomain, Round: 1, Proposal: optimization.Proposal{Rationale: "Expand tools"}, Rejection: "tool expansion is outside the policy"}
		change.Candidates = []optimization.Candidate{candidate, rejected}
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err != nil {
			t.Fatal(err)
		}
		stored, err := s.GetCandidate(ctx, rejected.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Accepted || stored.Rejection != rejected.Rejection || !stored.Version.ID.IsNil() {
			t.Fatalf("rejected proposal lost or became executable: %+v", stored)
		}
	})
	t.Run("CandidateCannotRebindStoredVersion", func(t *testing.T) {
		s := factory(t, time.Now)
		bundle := candidateBundle(t)
		ctx := scoped(bundle.Experiment.Spec.Scope)
		candidate, version := candidateVersion(t, bundle, "Check the evidence before answering.")
		original := version
		original.HarnessID = id.New(optimization.PrefixHarness)
		bundle.Versions = append(bundle.Versions, original)
		lease := proposalExperiment(ctx, t, s, bundle)
		change := candidateTransition(bundle, lease, candidate, version)
		if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); !errors.Is(err, optimization.ErrConflict) {
			t.Fatalf("existing version ID rebound to another harness: %v", err)
		}
		stored, err := s.GetVersion(ctx, version.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.HarnessID != original.HarnessID {
			t.Fatal("stored version identity changed")
		}
	})
	t.Run("ComposedCandidateCountsTowardLimit", func(t *testing.T) {
		for _, limit := range []int{2, 3} {
			t.Run(strconv.Itoa(limit), func(t *testing.T) {
				s := factory(t, time.Now)
				bundle := candidateBundle(t)
				bundle.Experiment.Spec.Limits.MaxCandidates = limit
				ctx := scoped(bundle.Experiment.Spec.Scope)
				lease := proposalExperiment(ctx, t, s, bundle)
				general, firstVersion := candidateVersion(t, bundle, "Check the evidence before answering.")
				domain, secondVersion := candidateVersion(t, bundle, "Check domain assumptions before answering.")
				domain.Origin = optimization.OriginDomain
				change := candidateTransition(bundle, lease, general, firstVersion)
				change.Candidates = append(change.Candidates, domain)
				change.Versions = append(change.Versions, secondVersion)
				if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) }); err != nil {
					t.Fatal(err)
				}
				if err := s.Transact(ctx, func(tx store.Tx) error {
					return tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 5, NextState: optimization.StateRunning, NextPhase: optimization.PhaseCompose, Lease: lease, IdempotencyKey: "compose", Actor: bundle.Experiment.Spec.Requester})
				}); err != nil {
					t.Fatal(err)
				}
				composed, thirdVersion := candidateVersion(t, bundle, "Check evidence and domain assumptions before answering.")
				composed.Origin = optimization.OriginComposed
				composed.Parents = []id.ID{general.ID, domain.ID}
				change = candidateTransition(bundle, lease, composed, thirdVersion)
				change.ExpectedRevision = 6
				change.IdempotencyKey = "composition-result"
				err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(change) })
				if limit == 2 {
					if !errors.Is(err, optimization.ErrBudgetExceeded) {
						t.Fatalf("composition escaped candidate cap: %v", err)
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				stored, err := s.GetCandidate(ctx, composed.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(stored.Parents) != 2 || stored.Parents[0] != general.ID || stored.Parents[1] != domain.ID {
					t.Fatal("composition lost parent provenance")
				}
			})
		}
	})
}

func candidateBundle(t *testing.T) optimization.ExperimentBundle {
	t.Helper()
	bundle := Fixture(t, "candidate")
	view, err := bundle.Versions[0].Prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Sections) == 0 {
		t.Fatal("fixture lacks an editable instruction section")
	}
	base := bundle.Versions[0]
	base.Policy.MutableSections = []string{view.Sections[0].ID}
	base, err = optimization.SealVersion(base)
	if err != nil {
		t.Fatal(err)
	}
	bundle.Versions[0] = base
	bundle.Experiment.Spec.Baseline.Digest = base.Digest
	return bundle
}

func proposalExperiment(ctx context.Context, t *testing.T, s store.Store, bundle optimization.ExperimentBundle) optimization.WorkLease {
	t.Helper()
	var lease optimization.WorkLease
	if err := s.Transact(ctx, func(tx store.Tx) error {
		if err := tx.CreateExperiment(bundle); err != nil {
			return err
		}
		if err := tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester}); err != nil {
			return err
		}
		var err error
		lease, err = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: bundle.Experiment.ID, Owner: "proposal-worker", Duration: 5 * time.Minute})
		if err != nil {
			return err
		}
		return tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 3, NextState: optimization.StateRunning, NextPhase: optimization.PhasePropose, IdempotencyKey: "propose", Lease: lease, Actor: bundle.Experiment.Spec.Requester})
	}); err != nil {
		t.Fatal(err)
	}
	return lease
}

func candidateVersion(t *testing.T, bundle optimization.ExperimentBundle, body string) (optimization.Candidate, optimization.HarnessVersion) {
	t.Helper()
	base := bundle.Versions[0]
	proposal := optimization.Proposal{Edits: []optimization.Edit{{Kind: optimization.EditSectionBody, Target: base.Policy.MutableSections[0], Text: body}}, Rationale: "Use an explicit evidence check"}
	version, err := optimization.ValidateProposal(base, proposal, base.Policy)
	if err != nil {
		t.Fatal(err)
	}
	candidate := optimization.Candidate{ID: id.New(optimization.PrefixCandidate), ExperimentID: bundle.Experiment.ID, Origin: optimization.OriginGeneral, Round: 1, Proposal: proposal, Version: optimization.VersionRef{ID: version.ID, Digest: version.Digest}, Accepted: true}
	return candidate, version
}

func candidateTransition(bundle optimization.ExperimentBundle, lease optimization.WorkLease, candidate optimization.Candidate, version optimization.HarnessVersion) optimization.Transition {
	return optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 4, NextState: optimization.StateRunning, NextPhase: optimization.PhaseProbe, Lease: lease, IdempotencyKey: "proposal-result", Actor: bundle.Experiment.Spec.Requester, Candidates: []optimization.Candidate{candidate}, Versions: []optimization.HarnessVersion{version}}
}
