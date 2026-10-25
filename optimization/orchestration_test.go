package optimization

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	rootsqlite "github.com/xraph/cortex/store/sqlite"
)

func freshOrchestrationFixture(t *testing.T) (context.Context, *rootsqlite.Store, *engine.Engine, *agent.Config, HarnessVersion) {
	t.Helper()
	ctx, sources, runtime, owner, version := compatibilityFixture(t)
	second := &agent.Config{ID: id.NewAgentID(), Name: "second", SystemPrompt: "Review the work.", Model: "model"}
	if err := sources.Create(ctx, second); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Hour)
	budgets := map[string]execution.Budget{"worker": {MaxSteps: 2, MaxTokens: 100, MaxToolCalls: 2, Deadline: deadline}, "second": {MaxSteps: 2, MaxTokens: 100, MaxToolCalls: 2, Deadline: deadline}}
	presets := make([]OrchestrationPreset, 0, 2)
	for _, strategy := range []string{orchestration.StrategySequential, orchestration.StrategyParallel} {
		cfg := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: strategy, Strategy: strategy, Participants: []orchestration.Participant{{AgentName: owner.Name}, {AgentName: second.Name}}}
		if err := sources.CreateOrchestration(ctx, cfg); err != nil {
			t.Fatal(err)
		}
		prepared, err := runtime.PrepareOrchestrationWithBudgets(ctx, cfg.ID, budgets)
		if err != nil {
			t.Fatalf("prepare %s: %v", strategy, err)
		}
		preset, err := SealOrchestrationPreset(OrchestrationPreset{Name: strategy, Prepared: prepared, Limits: execution.Resources{Steps: 4, Tokens: 200, ToolCalls: 4, FanOut: 2}, Guidance: map[string][]string{"worker": {"role"}}})
		if err != nil {
			t.Fatal(err)
		}
		presets = append(presets, preset)
	}
	version.Prepared = nil
	version.Policy = EditPolicy{OrchestrationPresets: presets, MaxBodyBytes: 128, MaxPatchBytes: 256, MaxEdits: 4}
	version.Orchestration = &OrchestrationSelection{Preset: presets[0].Name, Prepared: presets[0].Prepared}
	frozen, err := SealVersion(version)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, sources, runtime, owner, frozen
}

func orchestrationVersionFixture(t *testing.T) HarnessVersion {
	t.Helper()
	ctx, sources, _, owner, frozen := freshOrchestrationFixture(t)
	owner.SystemPrompt = "Live source changed."
	if err := sources.Update(ctx, owner); err != nil {
		t.Fatal(err)
	}
	return frozen
}

func TestPresetPinsEveryParticipant(t *testing.T) {
	version := orchestrationVersionFixture(t)
	before, err := json.Marshal(version)
	if err != nil {
		t.Fatal(err)
	}
	for _, preset := range version.Policy.OrchestrationPresets {
		view, inspectErr := preset.Prepared.Inspect()
		if inspectErr != nil || len(view.Participants) != 2 {
			t.Fatalf("frozen roster: %+v %v", view, inspectErr)
		}
		participant, inspectErr := view.Participants["worker"].Inspect()
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		for _, section := range participant.Sections {
			if section.Body == "Live source changed." {
				t.Fatal("preset reloaded a live participant")
			}
		}
		view.Config.Participants[0].AgentName = "external-peer"
	}
	after, err := json.Marshal(version)
	if err != nil || string(before) != string(after) {
		t.Fatal("inspection mutated preset")
	}
}

func TestApprovedPresetSelection(t *testing.T) {
	base := orchestrationVersionFixture(t)
	candidate, err := ValidateProposal(base, Proposal{Edits: []Edit{{Kind: EditOrchestrationPreset, Target: orchestration.StrategyParallel}}}, base.Policy)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Prepared != nil || candidate.Orchestration.Preset != orchestration.StrategyParallel || candidate.ParentID != base.ID || candidate.Digest == base.Digest || candidate.PolicyDigest != base.PolicyDigest {
		t.Fatalf("invalid selection: %+v", candidate)
	}
	if candidate.Orchestration.Prepared.Digest() != base.Policy.OrchestrationPresets[1].Prepared.Digest() {
		t.Fatal("selection changed the approved team")
	}
	if !reflect.DeepEqual(candidate.Policy, base.Policy) {
		t.Fatal("selection changed host policy")
	}
	if base.Orchestration.Preset != orchestration.StrategySequential {
		t.Fatal("selection mutated baseline")
	}
}

func TestUnapprovedPresetRefused(t *testing.T) {
	base := orchestrationVersionFixture(t)
	for _, proposal := range []Proposal{
		{Edits: []Edit{{Kind: EditOrchestrationPreset, Target: "external-team"}}},
		{Edits: []Edit{{Kind: EditOrchestrationPreset, Target: orchestration.StrategyParallel, Text: "replace roster"}}},
		{Edits: []Edit{{Kind: EditSectionBody, Target: "role", Text: "bypass team policy"}}},
	} {
		if _, err := ValidateProposal(base, proposal, base.Policy); err == nil {
			t.Fatal("accepted unapproved team edit")
		}
	}
	policy := base.Policy
	policy.OrchestrationPresets = append([]OrchestrationPreset(nil), policy.OrchestrationPresets...)
	policy.OrchestrationPresets[0].Limits.Tokens++
	if _, err := ValidateProposal(base, Proposal{Edits: []Edit{{Kind: EditOrchestrationPreset, Target: orchestration.StrategyParallel}}}, policy); err == nil {
		t.Fatal("accepted changed host ceiling")
	}
	ambiguous := base
	ambiguous.Prepared = fixtureVersion(t).Prepared
	if _, err := SealVersion(ambiguous); err == nil {
		t.Fatal("accepted agent and team together")
	}
	missing := base
	missing.Orchestration = &OrchestrationSelection{Preset: "external-team", Prepared: base.Orchestration.Prepared}
	if _, err := SealVersion(missing); !errors.Is(err, ErrPolicyDenied) {
		t.Fatalf("unapproved selected preset: %v", err)
	}
	foreign := base
	foreign.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "foreign"}}}
	if _, err := SealVersion(foreign); err == nil {
		t.Fatal("accepted foreign team scope")
	}
	mixedPolicy := base
	mixedPolicy.Policy.MutableSections = []string{"role"}
	if _, err := SealVersion(mixedPolicy); err == nil {
		t.Fatal("accepted ambiguous agent edit policy on a team")
	}
}

func TestPresetRejectsInvalidBoundsAndGuidance(t *testing.T) {
	base := orchestrationVersionFixture(t)
	for name, mutate := range map[string]func(*OrchestrationPreset){
		"missing-team":      func(p *OrchestrationPreset) { p.Prepared = nil },
		"tokens":            func(p *OrchestrationPreset) { p.Limits.Tokens = 99 },
		"steps":             func(p *OrchestrationPreset) { p.Limits.Steps = 0 },
		"fanout":            func(p *OrchestrationPreset) { p.Limits.FanOut = execution.MaxReservations + 1 },
		"money":             func(p *OrchestrationPreset) { p.Limits.USDMicros = 10 },
		"replans":           func(p *OrchestrationPreset) { p.Limits.Replans = 1 },
		"external-peer":     func(p *OrchestrationPreset) { p.Guidance = map[string][]string{"external": {"role"}} },
		"missing-section":   func(p *OrchestrationPreset) { p.Guidance = map[string][]string{"worker": {"unknown"}} },
		"duplicate-section": func(p *OrchestrationPreset) { p.Guidance = map[string][]string{"worker": {"role", "role"}} },
	} {
		t.Run(name, func(t *testing.T) {
			preset := base.Policy.OrchestrationPresets[0]
			mutate(&preset)
			if _, err := SealOrchestrationPreset(preset); err == nil {
				t.Fatal("accepted invalid preset")
			}
		})
	}
	duplicate := base
	duplicate.Policy.OrchestrationPresets = append(append([]OrchestrationPreset(nil), base.Policy.OrchestrationPresets...), base.Policy.OrchestrationPresets[0])
	if _, err := SealVersion(duplicate); err == nil {
		t.Fatal("accepted repeated preset name")
	}
	detached, err := SealVersion(base)
	if err != nil {
		t.Fatal(err)
	}
	detached.Policy.OrchestrationPresets[0].Guidance["worker"][0] = "mutated"
	if base.Policy.OrchestrationPresets[0].Guidance["worker"][0] != "role" {
		t.Fatal("sealed policy aliases caller")
	}
	if _, err := SealVersion(detached); err == nil {
		t.Fatal("accepted tampered preset digest")
	}
}

func TestPresetRequiresExplicitParticipantBudgets(t *testing.T) {
	ctx, sources, runtime, owner, _ := compatibilityFixture(t)
	cfg := &orchestration.Config{ID: id.NewOrchestrationConfigID(), Name: "unbounded", Strategy: orchestration.StrategySequential, Participants: []orchestration.Participant{{AgentName: owner.Name}}}
	if err := sources.CreateOrchestration(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	prepared, err := runtime.PrepareOrchestration(ctx, cfg.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = SealOrchestrationPreset(OrchestrationPreset{Name: "unbounded", Prepared: prepared, Limits: execution.Resources{Steps: 2, Tokens: 100, ToolCalls: 2, FanOut: 1}}); err == nil {
		t.Fatal("accepted unbounded participant")
	}
}

// The fixture was generated by published optimization commit eca1461, before
// orchestration fields were added to the version and policy wire formats.
func TestPresetFieldsPreserveLegacyAgentDigest(t *testing.T) {
	data, err := os.ReadFile("testdata/agent-version-eca1461.json")
	if err != nil {
		t.Fatal(err)
	}
	var legacy HarnessVersion
	if err = json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	sealed, err := SealVersion(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.Digest != legacy.Digest || sealed.PolicyDigest != legacy.PolicyDigest || sealed.Orchestration != nil || len(sealed.Policy.OrchestrationPresets) != 0 {
		t.Fatal("legacy agent content identity changed")
	}
	if _, err = ValidateProposal(sealed, Proposal{Edits: []Edit{{Kind: EditSectionBody, Target: "role", Text: "Refine the fixture instructions."}}}, sealed.Policy); err != nil {
		t.Fatalf("legacy proposal rejected: %v", err)
	}
}
