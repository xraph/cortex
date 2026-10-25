package optimization

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/orchestration"
)

func guidanceProposal(text string) Proposal {
	return Proposal{Edits: []Edit{{Kind: EditParticipantGuidance, Participant: "worker", Target: "role", Text: text}}}
}

func TestParticipantGuidancePreservesHostBounds(t *testing.T) {
	base := orchestrationVersionFixture(t)
	before, err := base.Orchestration.Prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	original, err := before.Participants["worker"].Inspect()
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := ValidateProposal(base, guidanceProposal("Delegate only the declared review task."), base.Policy)
	if err != nil {
		t.Fatal(err)
	}
	after, err := candidate.Orchestration.Prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	child, err := after.Participants["worker"].Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Config, after.Config) || before.Snapshot != after.Snapshot || !reflect.DeepEqual(original.Config, child.Config) || !reflect.DeepEqual(original.Tools, child.Tools) || original.AgentID != child.AgentID || original.Scope.Canonical() != child.Scope.Canonical() {
		t.Fatal("guidance changed host bounds")
	}
	if before.Participants["second"].Digest() != after.Participants["second"].Digest() || child.Provenance.DerivedFrom != before.Participants["worker"].Digest() {
		t.Fatal("guidance changed unrelated participant or provenance")
	}
	if candidate.Orchestration.Guidance["worker"]["role"] != "Delegate only the declared review task." || candidate.PolicyDigest != base.PolicyDigest {
		t.Fatal("guidance or policy binding lost")
	}
	changed := false
	for _, part := range child.Sections {
		if part.ID == "role" {
			changed = part.Body == "Delegate only the declared review task."
		}
	}
	if !changed {
		t.Fatal("declared guidance was not applied")
	}
	for _, mutation := range []engine.PreparedEdits{{Tools: &[]string{}}, {Temperature: new(.25)}, {ToolDescriptions: map[string]string{"read": "different tool guidance"}}} {
		replaced, deriveErr := base.Orchestration.Prepared.Derive(map[string]engine.PreparedEdits{"worker": mutation})
		if deriveErr != nil {
			t.Fatal(deriveErr)
		}
		forged := candidate
		copied := *candidate.Orchestration
		copied.Prepared = replaced
		forged.Orchestration = &copied
		if _, sealErr := SealVersion(forged); !errors.Is(sealErr, ErrPolicyDenied) {
			t.Fatalf("accepted undeclared runtime edit: %v", sealErr)
		}
	}
}

func TestParticipantGuidanceRejectsUndeclaredEdits(t *testing.T) {
	base := orchestrationVersionFixture(t)
	for name, edit := range map[string]Edit{
		"unknown-participant":    {Kind: EditParticipantGuidance, Participant: "external", Target: "role", Text: "edit"},
		"unapproved-participant": {Kind: EditParticipantGuidance, Participant: "second", Target: "role", Text: "edit"},
		"unknown-section":        {Kind: EditParticipantGuidance, Participant: "worker", Target: "unknown", Text: "edit"},
		"tools":                  {Kind: EditParticipantGuidance, Participant: "worker", Target: "role", Tools: &[]string{}},
		"sampling":               {Kind: EditParticipantGuidance, Participant: "worker", Target: "role", Number: new(.2)},
		"oversized":              {Kind: EditParticipantGuidance, Participant: "worker", Target: "role", Text: strings.Repeat("x", 129)},
		"agent-edit":             {Kind: EditSectionBody, Participant: "worker", Target: "role", Text: "edit"},
		"preset-participant":     {Kind: EditOrchestrationPreset, Participant: "worker", Target: orchestration.StrategyParallel},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ValidateProposal(base, Proposal{Edits: []Edit{edit}}, base.Policy); err == nil {
				t.Fatal("accepted undeclared guidance")
			}
		})
	}
	duplicate := guidanceProposal("edit")
	duplicate.Edits = append(duplicate.Edits, duplicate.Edits[0])
	if _, err := ValidateProposal(base, duplicate, base.Policy); err == nil {
		t.Fatal("accepted duplicate guidance target")
	}
	agent := fixtureVersion(t)
	if _, err := ValidateProposal(agent, Proposal{Edits: []Edit{{Kind: EditSectionBody, Participant: "worker", Target: "role", Text: "edit"}}}, agent.Policy); err == nil {
		t.Fatal("agent ignored participant qualifier")
	}
}

func TestGuidanceSelectionAndResetAreDeterministic(t *testing.T) {
	base := orchestrationVersionFixture(t)
	first := guidanceProposal("Review before delegation.")
	first.Edits = append(first.Edits, Edit{Kind: EditOrchestrationPreset, Target: orchestration.StrategyParallel})
	second := Proposal{Edits: []Edit{first.Edits[1], first.Edits[0]}}
	a, err := ValidateProposal(base, first, base.Policy)
	if err != nil {
		t.Fatal(err)
	}
	b, err := ValidateProposal(base, second, base.Policy)
	if err != nil || a.Digest != b.Digest {
		t.Fatalf("selection depends on edit order: %v", err)
	}
	again, err := ValidateProposal(a, guidanceProposal("Review before delegation."), a.Policy)
	if err != nil || again.Digest != a.Digest {
		t.Fatalf("same guidance changed provenance: %v", err)
	}
	original, err := base.Policy.OrchestrationPresets[1].Prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	child, err := original.Participants["worker"].Inspect()
	if err != nil {
		t.Fatal(err)
	}
	var originalBody string
	for _, part := range child.Sections {
		if part.ID == "role" {
			originalBody = part.Body
		}
	}
	reset, err := ValidateProposal(a, guidanceProposal(originalBody), a.Policy)
	if err != nil || len(reset.Orchestration.Guidance) != 0 || reset.Orchestration.Prepared.Digest() != base.Policy.OrchestrationPresets[1].Prepared.Digest() {
		t.Fatalf("guidance reset lost original preset: %+v %v", reset, err)
	}
	selected, err := ValidateProposal(a, Proposal{Edits: []Edit{{Kind: EditOrchestrationPreset, Target: orchestration.StrategySequential}}}, a.Policy)
	if err != nil || len(selected.Orchestration.Guidance) != 0 || selected.Digest != base.Digest {
		t.Fatalf("preset switch carried guidance: %v", err)
	}
}

func TestGuidanceWireBindsDeclaredEditsToPreparedContent(t *testing.T) {
	base := orchestrationVersionFixture(t)
	candidate, err := ValidateProposal(base, guidanceProposal("Review the delegated work."), base.Policy)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	var restored HarnessVersion
	if err = json.Unmarshal(data, &restored); err != nil {
		t.Fatal(err)
	}
	sealed, err := SealVersion(restored)
	if err != nil || sealed.Digest != candidate.Digest {
		t.Fatalf("wire changed derived team: %v", err)
	}
	restored.Orchestration.Guidance["worker"]["role"] = "Different declared edit."
	if _, err = SealVersion(restored); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("accepted changed guidance declaration: %v", err)
	}
	if candidate.Orchestration.Guidance["worker"]["role"] != "Review the delegated work." {
		t.Fatal("decoded guidance aliases original")
	}
	empty := base
	selection := *base.Orchestration
	selection.Guidance = map[string]map[string]string{"worker": {}}
	empty.Orchestration = &selection
	if _, err = SealVersion(empty); err == nil {
		t.Fatal("accepted redundant empty guidance")
	}
}
