package optimization

import (
	"strings"

	"github.com/xraph/cortex/id"
)

// ActivationChange records an explicit review command. The ID is its retry key;
// scope and creation time come from the transaction, and generation is assigned
// by the store. The service must recheck current grants and evidence availability.
type ActivationChange struct {
	Kind               HarnessKind `json:"kind,omitempty"`
	Reason             string      `json:"reason,omitempty"`
	ID                 id.ID       `json:"id"`
	AgentID            id.AgentID  `json:"agent_id"`
	Channel            string      `json:"channel"`
	Version            VersionRef  `json:"version"`
	ExpectedGeneration int64       `json:"expected_generation"`
	Actor              IdentityRef `json:"actor"`
	PolicyDigest       string      `json:"policy_digest"`
	DecisionID         id.ID       `json:"decision_id"`
	RollbackTo         id.ID       `json:"rollback_to,omitempty"`
}

// ActivationChannel resolves the default and validates a bounded channel name.
func ActivationChannel(channel string) (string, error) {
	if channel == "" {
		return "default", nil
	}
	if len(channel) > 64 || strings.ContainsFunc(channel, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.'
	}) {
		return "", ErrInvalidSpec
	}
	return channel, nil
}

// ActivationKind resolves legacy execution routes and rejects unsupported kinds.
func ActivationKind(kind HarnessKind) (HarnessKind, error) {
	switch kind {
	case "", HarnessExecution:
		return HarnessExecution, nil
	case HarnessImprovement:
		return HarnessImprovement, nil
	default:
		return "", ErrInvalidSpec
	}
}

// ValidateActivationEvidence binds an activation to its immutable recommendation.
// Current authorization, retained artifacts and source compatibility belong to
// the service; satisfying this check alone does not enable active execution.
func ValidateActivationEvidence(change ActivationChange, experiment Experiment, decision Decision, version HarnessVersion) error {
	if experiment.RetainedBaseline != nil {
		return ErrPolicyDenied
	}
	experimentScope, err := ScopeKey(experiment.Spec.Scope)
	if err != nil {
		return err
	}
	versionScope, err := ScopeKey(version.Scope)
	if err != nil {
		return err
	}
	if experimentScope != versionScope {
		return ErrIncompleteEvidence
	}
	if decision.ID != change.DecisionID || experiment.DecisionID != decision.ID || decision.ExperimentID != experiment.ID || decision.SpecDigest != experiment.SpecDigest || decision.Candidate != change.Version || version.ID != change.Version.ID || version.Digest != change.Version.Digest || experiment.Spec.AgentID != change.AgentID || version.AgentID != change.AgentID {
		return ErrIncompleteEvidence
	}
	if decision.Outcome != DecisionRecommend || decision.EvidenceClass != EvidenceValidated || !decision.Comparison.Complete || experiment.Spec.Mode != ModeSequential && experiment.Spec.Mode != ModeParallel {
		return ErrIncompleteEvidence
	}
	kind, err := ActivationKind(change.Kind)
	if err != nil {
		return err
	}
	if decision.SelectionKind != string(kind) || version.Kind != kind {
		return ErrIncompleteEvidence
	}
	if kind == HarnessImprovement {
		if experiment.Spec.Improvement == nil || experiment.State != StateReviewRequired || decision.Baseline != experiment.Spec.Improvement.Controllers[0] || decision.Candidate != experiment.Spec.Improvement.Controllers[1] {
			return ErrIncompleteEvidence
		}
	} else if experiment.Spec.Improvement != nil {
		return ErrIncompleteEvidence
	}
	if change.PolicyDigest != decision.PolicyDigest || change.PolicyDigest != version.PolicyDigest {
		return ErrPolicyDenied
	}
	return nil
}
