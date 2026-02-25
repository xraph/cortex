package id_test

import (
	"strings"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestConstructors(t *testing.T) {
	tests := []struct {
		name   string
		newFn  func() id.ID
		prefix string
	}{
		{"AgentID", id.NewAgentID, "agt_"},
		{"AgentRunID", id.NewAgentRunID, "arun_"},
		{"ToolID", id.NewToolID, "tool_"},
		{"ToolCallID", id.NewToolCallID, "tcall_"},
		{"StepID", id.NewStepID, "astp_"},
		{"MemoryID", id.NewMemoryID, "mem_"},
		{"CheckpointID", id.NewCheckpointID, "acp_"},
		{"OrchestrationID", id.NewOrchestrationID, "orch_"},
		{"SkillID", id.NewSkillID, "skl_"},
		{"TraitID", id.NewTraitID, "trt_"},
		{"BehaviorID", id.NewBehaviorID, "bhv_"},
		{"PersonaID", id.NewPersonaID, "prs_"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.newFn().String()
			if !strings.HasPrefix(got, tt.prefix) {
				t.Errorf("expected prefix %q, got %q", tt.prefix, got)
			}
		})
	}
}

func TestNew(t *testing.T) {
	i := id.New(id.PrefixAgent)
	if i.IsNil() {
		t.Fatal("expected non-nil ID")
	}
	if i.Prefix() != id.PrefixAgent {
		t.Errorf("expected prefix %q, got %q", id.PrefixAgent, i.Prefix())
	}
}

func TestParseRoundTrip(t *testing.T) {
	tests := []struct {
		name    string
		newFn   func() id.ID
		parseFn func(string) (id.ID, error)
	}{
		{"AgentID", id.NewAgentID, id.ParseAgentID},
		{"AgentRunID", id.NewAgentRunID, id.ParseAgentRunID},
		{"ToolID", id.NewToolID, id.ParseToolID},
		{"ToolCallID", id.NewToolCallID, id.ParseToolCallID},
		{"StepID", id.NewStepID, id.ParseStepID},
		{"MemoryID", id.NewMemoryID, id.ParseMemoryID},
		{"CheckpointID", id.NewCheckpointID, id.ParseCheckpointID},
		{"OrchestrationID", id.NewOrchestrationID, id.ParseOrchestrationID},
		{"SkillID", id.NewSkillID, id.ParseSkillID},
		{"TraitID", id.NewTraitID, id.ParseTraitID},
		{"BehaviorID", id.NewBehaviorID, id.ParseBehaviorID},
		{"PersonaID", id.NewPersonaID, id.ParsePersonaID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := tt.newFn()
			parsed, err := tt.parseFn(original.String())
			if err != nil {
				t.Fatalf("parse failed: %v", err)
			}
			if parsed.String() != original.String() {
				t.Errorf("round-trip mismatch: %q != %q", parsed.String(), original.String())
			}
		})
	}
}

func TestCrossTypeRejection(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		parseFn func(string) (id.ID, error)
	}{
		{"ParseAgentID rejects arun_", id.NewAgentRunID().String(), id.ParseAgentID},
		{"ParseAgentRunID rejects tool_", id.NewToolID().String(), id.ParseAgentRunID},
		{"ParseToolID rejects tcall_", id.NewToolCallID().String(), id.ParseToolID},
		{"ParseToolCallID rejects astp_", id.NewStepID().String(), id.ParseToolCallID},
		{"ParseStepID rejects mem_", id.NewMemoryID().String(), id.ParseStepID},
		{"ParseMemoryID rejects acp_", id.NewCheckpointID().String(), id.ParseMemoryID},
		{"ParseCheckpointID rejects orch_", id.NewOrchestrationID().String(), id.ParseCheckpointID},
		{"ParseOrchestrationID rejects skl_", id.NewSkillID().String(), id.ParseOrchestrationID},
		{"ParseSkillID rejects trt_", id.NewTraitID().String(), id.ParseSkillID},
		{"ParseTraitID rejects bhv_", id.NewBehaviorID().String(), id.ParseTraitID},
		{"ParseBehaviorID rejects prs_", id.NewPersonaID().String(), id.ParseBehaviorID},
		{"ParsePersonaID rejects agt_", id.NewAgentID().String(), id.ParsePersonaID},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.parseFn(tt.input)
			if err == nil {
				t.Errorf("expected error for cross-type parse of %q, got nil", tt.input)
			}
		})
	}
}

func TestParseAny(t *testing.T) {
	ids := []id.ID{
		id.NewAgentID(),
		id.NewAgentRunID(),
		id.NewToolID(),
		id.NewToolCallID(),
		id.NewStepID(),
		id.NewMemoryID(),
		id.NewCheckpointID(),
		id.NewOrchestrationID(),
		id.NewSkillID(),
		id.NewTraitID(),
		id.NewBehaviorID(),
		id.NewPersonaID(),
	}

	for _, i := range ids {
		t.Run(i.String(), func(t *testing.T) {
			parsed, err := id.ParseAny(i.String())
			if err != nil {
				t.Fatalf("ParseAny(%q) failed: %v", i.String(), err)
			}
			if parsed.String() != i.String() {
				t.Errorf("round-trip mismatch: %q != %q", parsed.String(), i.String())
			}
		})
	}
}

func TestParseWithPrefix(t *testing.T) {
	i := id.NewAgentID()
	parsed, err := id.ParseWithPrefix(i.String(), id.PrefixAgent)
	if err != nil {
		t.Fatalf("ParseWithPrefix failed: %v", err)
	}
	if parsed.String() != i.String() {
		t.Errorf("mismatch: %q != %q", parsed.String(), i.String())
	}

	_, err = id.ParseWithPrefix(i.String(), id.PrefixSkill)
	if err == nil {
		t.Error("expected error for wrong prefix")
	}
}

func TestParseEmpty(t *testing.T) {
	_, err := id.Parse("")
	if err == nil {
		t.Error("expected error for empty string")
	}
}

func TestNilID(t *testing.T) {
	var i id.ID
	if !i.IsNil() {
		t.Error("zero-value ID should be nil")
	}
	if i.String() != "" {
		t.Errorf("expected empty string, got %q", i.String())
	}
	if i.Prefix() != "" {
		t.Errorf("expected empty prefix, got %q", i.Prefix())
	}
}

func TestMarshalUnmarshalText(t *testing.T) {
	original := id.NewAgentID()
	data, err := original.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText failed: %v", err)
	}

	var restored id.ID
	if unmarshalErr := restored.UnmarshalText(data); unmarshalErr != nil {
		t.Fatalf("UnmarshalText failed: %v", unmarshalErr)
	}
	if restored.String() != original.String() {
		t.Errorf("mismatch: %q != %q", restored.String(), original.String())
	}

	// Nil round-trip.
	var nilID id.ID
	data, err = nilID.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText(nil) failed: %v", err)
	}
	var restored2 id.ID
	if err := restored2.UnmarshalText(data); err != nil {
		t.Fatalf("UnmarshalText(nil) failed: %v", err)
	}
	if !restored2.IsNil() {
		t.Error("expected nil after round-trip of nil ID")
	}
}

func TestValueScan(t *testing.T) {
	original := id.NewSkillID()
	val, err := original.Value()
	if err != nil {
		t.Fatalf("Value failed: %v", err)
	}

	var scanned id.ID
	if scanErr := scanned.Scan(val); scanErr != nil {
		t.Fatalf("Scan failed: %v", scanErr)
	}
	if scanned.String() != original.String() {
		t.Errorf("mismatch: %q != %q", scanned.String(), original.String())
	}

	// Nil round-trip.
	var nilID id.ID
	val, err = nilID.Value()
	if err != nil {
		t.Fatalf("Value(nil) failed: %v", err)
	}
	if val != nil {
		t.Errorf("expected nil value for nil ID, got %v", val)
	}

	var scanned2 id.ID
	if err := scanned2.Scan(nil); err != nil {
		t.Fatalf("Scan(nil) failed: %v", err)
	}
	if !scanned2.IsNil() {
		t.Error("expected nil after scan of nil")
	}
}

func TestUniqueness(t *testing.T) {
	a := id.NewAgentID()
	b := id.NewAgentID()
	if a.String() == b.String() {
		t.Errorf("two consecutive NewAgentID() calls returned the same ID: %q", a.String())
	}
}
