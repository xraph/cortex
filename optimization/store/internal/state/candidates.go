package state

import (
	"context"
	"strings"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

// GetCandidate reads an immutable accepted or rejected proposal in exact scope.
func (s *Store) GetCandidate(ctx context.Context, candidateID id.ID) (optimization.Candidate, error) {
	var empty optimization.Candidate
	key, err := contextScope(ctx)
	if err != nil {
		return empty, err
	}
	unlock := s.lockOperation()
	defer unlock()
	if s.closed {
		return empty, optimization.ErrUnavailable
	}
	data, loadErr := s.loadScope(ctx, key)
	if loadErr != nil {
		return empty, loadErr
	}
	if data != nil {
		if candidate, exists := data.Candidates[candidateID.String()]; exists {
			return clone(candidate)
		}
	}
	return empty, optimization.ErrNotFound
}

type candidateResources struct {
	candidates map[string]optimization.Candidate
	versions   map[string]optimization.HarnessVersion
}

func (tx *transaction) prepareCandidates(experiment optimization.Experiment, change optimization.Transition) (candidateResources, error) {
	var empty candidateResources
	if len(change.Candidates) == 0 {
		if len(change.Versions) != 0 {
			return empty, optimization.ErrInvalidSpec
		}
		return empty, nil
	}
	if experiment.State != optimization.StateRunning || change.NextState != optimization.StateRunning || change.NextPhase != optimization.PhaseProbe || (experiment.Phase != optimization.PhasePropose && experiment.Phase != optimization.PhaseCompose) {
		return empty, optimization.ErrConflict
	}
	versions, err := tx.validateVersions(experiment.Spec.AgentID, change.Versions)
	if err != nil {
		return empty, err
	}
	base := tx.data.Versions[experiment.Spec.Baseline.ID.String()]
	digests := map[string]bool{base.Digest: true}
	count := 0
	for _, candidate := range tx.data.Candidates {
		if candidate.ExperimentID == experiment.ID {
			count++
			if candidate.Accepted {
				digests[candidate.Version.Digest] = true
			}
		}
	}
	if len(change.Candidates) > experiment.Spec.Limits.MaxCandidates-count {
		return empty, optimization.ErrBudgetExceeded
	}
	result := candidateResources{candidates: make(map[string]optimization.Candidate), versions: versions}
	usedVersions := make(map[string]bool)
	for _, candidate := range change.Candidates {
		if _, parseErr := optimization.ParseID(candidate.ID.String(), optimization.PrefixCandidate); parseErr != nil {
			return empty, parseErr
		}
		if candidate.ExperimentID != experiment.ID || candidate.Round != experiment.Round || candidate.Round < 1 {
			return empty, optimization.ErrInvalidSpec
		}
		if _, exists := tx.data.Candidates[candidate.ID.String()]; exists {
			return empty, optimization.ErrConflict
		}
		if _, exists := result.candidates[candidate.ID.String()]; exists {
			return empty, optimization.ErrConflict
		}
		if parentErr := tx.validateCandidateParents(experiment, candidate); parentErr != nil {
			return empty, parentErr
		}
		if evidenceErr := tx.validateCandidateEvidence(experiment, candidate); evidenceErr != nil {
			return empty, evidenceErr
		}
		if candidate.Accepted {
			if candidate.Rejection != "" {
				return empty, optimization.ErrInvalidSpec
			}
			version, exists := versions[candidate.Version.ID.String()]
			if !exists {
				version, exists = tx.data.Versions[candidate.Version.ID.String()]
			}
			if !exists || version.Digest != candidate.Version.Digest || version.HarnessID != base.HarnessID || version.ParentID != base.ID || version.PolicyDigest != base.PolicyDigest {
				return empty, optimization.ErrStaleVersion
			}
			derived, deriveErr := optimization.ValidateProposal(base, candidate.Proposal, base.Policy)
			if deriveErr != nil {
				return empty, deriveErr
			}
			if derived.Digest != version.Digest {
				return empty, optimization.ErrStaleVersion
			}
			if digests[version.Digest] {
				return empty, optimization.ErrConflict
			}
			digests[version.Digest] = true
			usedVersions[version.ID.String()] = true
		} else if strings.TrimSpace(candidate.Rejection) == "" || !candidate.Version.ID.IsNil() || candidate.Version.Digest != "" {
			return empty, optimization.ErrInvalidSpec
		}
		candidate.CreatedAt = tx.now
		detached, cloneErr := clone(candidate)
		if cloneErr != nil {
			return empty, cloneErr
		}
		result.candidates[candidate.ID.String()] = detached
	}
	for versionID, version := range result.versions {
		if !usedVersions[versionID] {
			return empty, optimization.ErrInvalidSpec
		}
		version.CreatedAt = tx.now
		version.CreatedBy = change.Actor.Subject
		result.versions[versionID] = version
	}
	return result, nil
}

func (tx *transaction) validateCandidateParents(experiment optimization.Experiment, candidate optimization.Candidate) error {
	switch candidate.Origin {
	case optimization.OriginGeneral, optimization.OriginDomain:
		if experiment.Phase != optimization.PhasePropose || len(candidate.Parents) > 1 {
			return optimization.ErrInvalidSpec
		}
	case optimization.OriginComposed:
		if experiment.Phase != optimization.PhaseCompose || len(candidate.Parents) < 2 {
			return optimization.ErrInvalidSpec
		}
	default:
		return optimization.ErrInvalidSpec
	}
	seen := make(map[string]bool)
	for _, parentID := range candidate.Parents {
		parent, exists := tx.data.Candidates[parentID.String()]
		if !exists || parent.ExperimentID != experiment.ID || !parent.Accepted || parent.Round > candidate.Round {
			return optimization.ErrNotFound
		}
		if seen[parentID.String()] {
			return optimization.ErrInvalidSpec
		}
		seen[parentID.String()] = true
	}
	return nil
}

// Candidate evidence is restricted to persisted discovery results from this experiment.
// Incomplete scores remain useful diagnostics, but never become validation evidence.
func (tx *transaction) validateCandidateEvidence(experiment optimization.Experiment, candidate optimization.Candidate) error {
	seen := make(map[string]bool, len(candidate.Proposal.EvidenceIDs))
	for _, trialID := range candidate.Proposal.EvidenceIDs {
		key := trialID.String()
		if seen[key] {
			return optimization.ErrInvalidSpec
		}
		seen[key] = true
		trial, exists := tx.data.Trials[key]
		if !exists || trial.ExperimentID != experiment.ID {
			return optimization.ErrNotFound
		}
		if trial.Split != optimization.SplitDiscovery {
			return optimization.ErrPolicyDenied
		}
		if trial.Execution == nil || trial.EvaluationID == "" {
			return optimization.ErrIncompleteEvidence
		}
		evaluation, exists := tx.data.Evaluations[trial.EvaluationID]
		if !exists || evaluation.TrialID != trial.ID || evaluation.ExecutionDigest != trial.Execution.Digest {
			return optimization.ErrIncompleteEvidence
		}
	}
	return nil
}
