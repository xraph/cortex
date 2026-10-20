package optimization

import (
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

func TestActivationEvidenceEligibility(t *testing.T) {
	agentID := id.NewAgentID()
	ref := VersionRef{ID: id.New(PrefixVersion), Digest: strings.Repeat("a", 64)}
	decisionID := id.New(PrefixDecision)
	experimentID := id.New(PrefixExperiment)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "test"}}}
	request := ActivationChange{AgentID: agentID, Version: ref, DecisionID: decisionID, PolicyDigest: strings.Repeat("b", 64)}
	experiment := Experiment{ID: experimentID, DecisionID: decisionID, SpecDigest: strings.Repeat("c", 64), Spec: ExperimentSpec{Scope: scope, AgentID: agentID, Mode: ModeSequential}}
	decision := Decision{ID: decisionID, ExperimentID: experimentID, SpecDigest: experiment.SpecDigest, Candidate: ref, Outcome: DecisionRecommend, EvidenceClass: EvidenceValidated, SelectionKind: string(HarnessExecution), PolicyDigest: request.PolicyDigest, Comparison: Comparison{Complete: true}}
	version := HarnessVersion{ID: ref.ID, Scope: scope, AgentID: agentID, Digest: ref.Digest, Kind: HarnessExecution, PolicyDigest: request.PolicyDigest}
	if err := ValidateActivationEvidence(request, experiment, decision, version); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"scope", "empty-scope", "task-local", "controller", "retain", "reject", "contrastive", "exploratory", "incomplete", "spec", "linked-decision", "version-policy"} {
		t.Run(mode, func(t *testing.T) {
			e, d, v := experiment, decision, version
			switch mode {
			case "scope":
				v.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "other"}}}
			case "empty-scope":
				e.Spec.Scope = cortex.Scope{}
				v.Scope = cortex.Scope{}
			case "task-local":
				e.Spec.Mode = ModeTaskLocal
			case "controller":
				v.Kind = HarnessImprovement
			case "retain":
				d.Outcome = DecisionRetain
			case "reject":
				d.Outcome = DecisionReject
			case "contrastive":
				d.EvidenceClass = EvidenceContrastive
			case "exploratory":
				d.EvidenceClass = EvidenceExploratory
			case "incomplete":
				d.Comparison.Complete = false
			case "spec":
				d.SpecDigest = strings.Repeat("d", 64)
			case "linked-decision":
				e.DecisionID = id.New(PrefixDecision)
			case "version-policy":
				v.PolicyDigest = strings.Repeat("d", 64)
			}
			if err := ValidateActivationEvidence(request, e, d, v); err == nil {
				t.Fatalf("accepted %s", mode)
			}
		})
	}
}

func TestActivationChannelIdentity(t *testing.T) {
	for _, channel := range []string{"", "default", "preview.v2", "release-1", "A_B"} {
		got, err := ActivationChannel(channel)
		if err != nil || got == "" || channel != "" && got != channel {
			t.Fatalf("valid channel %q: %q %v", channel, got, err)
		}
	}
	for _, channel := range []string{" ", " default", "a/b", "a\n", "é", strings.Repeat("a", 65)} {
		if _, err := ActivationChannel(channel); err == nil {
			t.Fatalf("invalid channel %q", channel)
		}
	}
}
