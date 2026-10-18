package optimization

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/store/scopespy"
)

func fixtureVersion(t *testing.T) HarnessVersion {
	t.Helper()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "test"}}}
	eng, err := engine.New(engine.WithStore(scopespy.New()), engine.WithTool(llm.Tool{Name: "read", Description: "Read fixture"}, func(context.Context, cortex.Invocation) (string, error) { return "fixture", nil }), engine.WithToolRevision("read", "fixture-v1"), engine.WithModelRevision("test-model", "fixture-v1"))
	if err != nil {
		t.Fatal(err)
	}
	agentID := id.NewAgentID()
	prepared, err := eng.PrepareAgent(cortex.WithScope(context.Background(), scope), agentID, &engine.RunOverrides{SystemPrompt: "Solve the task", Model: "test-model", Tools: []string{"read"}, ToolsRestricted: true})
	if err != nil {
		t.Fatal(err)
	}
	version, err := SealVersion(HarnessVersion{ID: id.New(PrefixVersion), HarnessID: id.New(PrefixHarness), SchemaVersion: 1, Kind: HarnessExecution, Scope: scope, AgentID: agentID, Prepared: prepared, RuntimeVersion: "v1.8.0", Policy: EditPolicy{MutableSections: []string{"role"}, MutableToolDescriptions: []string{"read"}, AllowToolSubset: true, MaxBodyBytes: 128, MaxPatchBytes: 256, MaxEdits: 4}})
	if err != nil {
		t.Fatal(err)
	}
	return version
}

func TestVersionCanonicalDigest(t *testing.T) {
	base := fixtureVersion(t)
	original, err := CanonicalVersion(base)
	if err != nil {
		t.Fatal(err)
	}
	changed := base
	changed.Name = "Renamed"
	changed.CreatedAt = time.Now()
	changed.ID = id.New(PrefixVersion)
	again, err := CanonicalVersion(changed)
	if err != nil {
		t.Fatal(err)
	}
	if string(original) != string(again) {
		t.Fatal("display or persistence metadata changed execution content")
	}
	controller := HarnessVersion{SchemaVersion: 1, Kind: HarnessImprovement, Scope: base.Scope, AgentID: base.AgentID, Policy: base.Policy, Controller: &ControllerSpec{Model: "test-model", ModelRevision: "v1", Sections: []prompt.Section{{ID: "first", Body: "A"}, {ID: "second", Body: "B"}}, Tools: []string{}}}
	before, err := CanonicalVersion(controller)
	if err != nil {
		t.Fatal(err)
	}
	controller.Controller.Sections[0], controller.Controller.Sections[1] = controller.Controller.Sections[1], controller.Controller.Sections[0]
	after, err := CanonicalVersion(controller)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) == string(after) {
		t.Fatal("ordered sections lost from content identity")
	}
	controller.Controller.Sections[1].ID = controller.Controller.Sections[0].ID
	if _, err = CanonicalVersion(controller); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("duplicate section accepted: %v", err)
	}
}

func TestProposalCannotChangeHostPolicy(t *testing.T) {
	base := fixtureVersion(t)
	for _, raw := range []string{
		`{"edits":[],"scope":{"levels":[]}}`,
		`{"edits":[],"model":"replacement"}`,
		`{"edits":[],"policy":{"allow_tool_subset":false}}`,
		`{"edits":[{"kind":"budget","number":99999}]}`,
		`{"edits":[{"kind":"tool_subset","tools":["read","write"]}]}`,
	} {
		t.Run(raw, func(t *testing.T) {
			proposal, err := DecodeProposal([]byte(raw))
			if err == nil {
				_, err = ValidateProposal(base, proposal, base.Policy)
			}
			if err == nil {
				t.Fatal("host policy escaped proposal validation")
			}
		})
	}
	altered := base.Policy
	altered.MaxPatchBytes++
	if _, err := ValidateProposal(base, Proposal{Edits: []Edit{{Kind: EditSectionBody, Target: "role", Text: "Better instructions"}}}, altered); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("changed host policy accepted: %v", err)
	}
}

func TestProposalRejectsUnknownAndDuplicateFields(t *testing.T) {
	for _, raw := range []string{
		`{"edits":[],"edits":[]}`,
		`{"edits":[{"kind":"section_body","target":"role","target":"other","text":"x"}]}`,
		`{"edits":[{"kind":"section_body","target":"role","text":"x","credentials":"secret"}]}`,
	} {
		if _, err := DecodeProposal([]byte(raw)); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("accepted ambiguous proposal %s: %v", raw, err)
		}
	}
	base := fixtureVersion(t)
	duplicate := Proposal{Edits: []Edit{{Kind: EditSectionBody, Target: "role", Text: "A"}, {Kind: EditSectionBody, Target: "role", Text: "B"}}}
	if _, err := ValidateProposal(base, duplicate, base.Policy); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("duplicate target accepted: %v", err)
	}
}

func TestProposalKeepsEmptyToolsAndBaselineImmutable(t *testing.T) {
	base := fixtureVersion(t)
	before, err := json.Marshal(base.Prepared)
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := DecodeProposal([]byte(`{"edits":[{"kind":"section_body","target":"role","text":"Use a concise answer"},{"kind":"tool_subset","tools":[]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := ValidateProposal(base, proposal, base.Policy)
	if err != nil {
		t.Fatal(err)
	}
	view, err := candidate.Prepared.Inspect()
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Tools) != 0 || !view.Config.AllToolsRestricted {
		t.Fatal("empty allowlist became unrestricted")
	}
	after, err := json.Marshal(base.Prepared)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) || candidate.Digest == base.Digest {
		t.Fatal("baseline mutated or candidate content did not change")
	}
	bad := math.NaN()
	if _, err = ValidateProposal(base, Proposal{Edits: []Edit{{Kind: EditTemperature, Number: &bad}}}, base.Policy); err == nil {
		t.Fatal("nonfinite sampling accepted")
	}
}

func TestVersionRejectsAmbiguousPreparedScope(t *testing.T) {
	base := fixtureVersion(t)
	eng, err := engine.New(engine.WithStore(scopespy.New()))
	if err != nil {
		t.Fatal(err)
	}
	ambiguous := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "test/project=two"}}}
	base.Prepared, err = eng.PrepareAgent(cortex.WithScope(context.Background(), ambiguous), base.AgentID, nil)
	if err != nil {
		t.Fatal(err)
	}
	base.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "test"}, {Key: "project", Value: "two"}}}
	if _, err = SealVersion(base); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("prepared scope alias accepted: %v", err)
	}
}

func TestControllerRequiresExplicitToolSelection(t *testing.T) {
	base := fixtureVersion(t)
	version := HarnessVersion{SchemaVersion: 1, Kind: HarnessImprovement, Scope: base.Scope, AgentID: base.AgentID, Policy: base.Policy, Controller: &ControllerSpec{Model: "test", Sections: []prompt.Section{{ID: "instructions", Body: "Propose"}}}}
	if _, err := CanonicalVersion(version); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("unspecified controller tools accepted: %v", err)
	}
	version.Controller.Tools = []string{}
	if _, err := CanonicalVersion(version); err != nil {
		t.Fatalf("explicit deny-all refused: %v", err)
	}
}
