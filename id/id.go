// Package id defines TypeID-based identity types for all Cortex entities.
//
// Every entity in Cortex uses a single ID struct with a prefix that identifies
// the entity type. IDs are K-sortable (UUIDv7-based), globally unique,
// and URL-safe in the format "prefix_suffix".
package id

import (
	"database/sql/driver"
	"fmt"

	"go.jetify.com/typeid/v2"
)

// Prefix identifies the entity type encoded in a TypeID.
type Prefix string

// Prefix constants for all Cortex entity types.
const (
	PrefixAgent         Prefix = "agt"
	PrefixAgentRun      Prefix = "arun"
	PrefixTool          Prefix = "tool"
	PrefixToolCall      Prefix = "tcall"
	PrefixStep          Prefix = "astp"
	PrefixMemory        Prefix = "mem"
	PrefixCheckpoint    Prefix = "acp"
	PrefixOrchestration Prefix = "orch"
	PrefixSkill         Prefix = "skl"
	PrefixTrait         Prefix = "trt"
	PrefixBehavior      Prefix = "bhv"
	PrefixPersona       Prefix = "prs"
)

// ID is the primary identifier type for all Cortex entities.
// It wraps a TypeID providing a prefix-qualified, globally unique,
// sortable, URL-safe identifier in the format "prefix_suffix".
//
//nolint:recvcheck // Value receivers for read-only methods, pointer receivers for UnmarshalText/Scan.
type ID struct {
	inner typeid.TypeID
	valid bool
}

// Nil is the zero-value ID.
var Nil ID

// New generates a new globally unique ID with the given prefix.
// It panics if prefix is not a valid TypeID prefix (programming error).
func New(prefix Prefix) ID {
	tid, err := typeid.Generate(string(prefix))
	if err != nil {
		panic(fmt.Sprintf("id: invalid prefix %q: %v", prefix, err))
	}

	return ID{inner: tid, valid: true}
}

// Parse parses a TypeID string (e.g., "agt_01h2xcejqtf2nbrexx3vqjhp41")
// into an ID. Returns an error if the string is not valid.
func Parse(s string) (ID, error) {
	if s == "" {
		return Nil, fmt.Errorf("id: parse %q: empty string", s)
	}

	tid, err := typeid.Parse(s)
	if err != nil {
		return Nil, fmt.Errorf("id: parse %q: %w", s, err)
	}

	return ID{inner: tid, valid: true}, nil
}

// ParseWithPrefix parses a TypeID string and validates that its prefix
// matches the expected value.
func ParseWithPrefix(s string, expected Prefix) (ID, error) {
	parsed, err := Parse(s)
	if err != nil {
		return Nil, err
	}

	if parsed.Prefix() != expected {
		return Nil, fmt.Errorf("id: expected prefix %q, got %q", expected, parsed.Prefix())
	}

	return parsed, nil
}

// MustParse is like Parse but panics on error. Use for hardcoded ID values.
func MustParse(s string) ID {
	parsed, err := Parse(s)
	if err != nil {
		panic(fmt.Sprintf("id: must parse %q: %v", s, err))
	}

	return parsed
}

// MustParseWithPrefix is like ParseWithPrefix but panics on error.
func MustParseWithPrefix(s string, expected Prefix) ID {
	parsed, err := ParseWithPrefix(s, expected)
	if err != nil {
		panic(fmt.Sprintf("id: must parse with prefix %q: %v", expected, err))
	}

	return parsed
}

// ──────────────────────────────────────────────────
// Type aliases for backward compatibility
// ──────────────────────────────────────────────────

// AgentID is a type-safe identifier for agents (prefix: "agt").
type AgentID = ID

// AgentRunID is a type-safe identifier for agent runs (prefix: "arun").
type AgentRunID = ID

// ToolID is a type-safe identifier for tools (prefix: "tool").
type ToolID = ID

// ToolCallID is a type-safe identifier for tool calls (prefix: "tcall").
type ToolCallID = ID

// StepID is a type-safe identifier for steps (prefix: "astp").
type StepID = ID

// MemoryID is a type-safe identifier for memories (prefix: "mem").
type MemoryID = ID

// CheckpointID is a type-safe identifier for checkpoints (prefix: "acp").
type CheckpointID = ID

// OrchestrationID is a type-safe identifier for orchestrations (prefix: "orch").
type OrchestrationID = ID

// SkillID is a type-safe identifier for skills (prefix: "skl").
type SkillID = ID

// TraitID is a type-safe identifier for traits (prefix: "trt").
type TraitID = ID

// BehaviorID is a type-safe identifier for behaviors (prefix: "bhv").
type BehaviorID = ID

// PersonaID is a type-safe identifier for personas (prefix: "prs").
type PersonaID = ID

// AnyID is a type alias that accepts any valid prefix.
type AnyID = ID

// ──────────────────────────────────────────────────
// Convenience constructors
// ──────────────────────────────────────────────────

// NewAgentID generates a new unique agent ID.
func NewAgentID() ID { return New(PrefixAgent) }

// NewAgentRunID generates a new unique agent run ID.
func NewAgentRunID() ID { return New(PrefixAgentRun) }

// NewToolID generates a new unique tool ID.
func NewToolID() ID { return New(PrefixTool) }

// NewToolCallID generates a new unique tool call ID.
func NewToolCallID() ID { return New(PrefixToolCall) }

// NewStepID generates a new unique step ID.
func NewStepID() ID { return New(PrefixStep) }

// NewMemoryID generates a new unique memory ID.
func NewMemoryID() ID { return New(PrefixMemory) }

// NewCheckpointID generates a new unique checkpoint ID.
func NewCheckpointID() ID { return New(PrefixCheckpoint) }

// NewOrchestrationID generates a new unique orchestration ID.
func NewOrchestrationID() ID { return New(PrefixOrchestration) }

// NewSkillID generates a new unique skill ID.
func NewSkillID() ID { return New(PrefixSkill) }

// NewTraitID generates a new unique trait ID.
func NewTraitID() ID { return New(PrefixTrait) }

// NewBehaviorID generates a new unique behavior ID.
func NewBehaviorID() ID { return New(PrefixBehavior) }

// NewPersonaID generates a new unique persona ID.
func NewPersonaID() ID { return New(PrefixPersona) }

// ──────────────────────────────────────────────────
// Convenience parsers
// ──────────────────────────────────────────────────

// ParseAgentID parses a string and validates the "agt" prefix.
func ParseAgentID(s string) (ID, error) { return ParseWithPrefix(s, PrefixAgent) }

// ParseAgentRunID parses a string and validates the "arun" prefix.
func ParseAgentRunID(s string) (ID, error) { return ParseWithPrefix(s, PrefixAgentRun) }

// ParseToolID parses a string and validates the "tool" prefix.
func ParseToolID(s string) (ID, error) { return ParseWithPrefix(s, PrefixTool) }

// ParseToolCallID parses a string and validates the "tcall" prefix.
func ParseToolCallID(s string) (ID, error) { return ParseWithPrefix(s, PrefixToolCall) }

// ParseStepID parses a string and validates the "astp" prefix.
func ParseStepID(s string) (ID, error) { return ParseWithPrefix(s, PrefixStep) }

// ParseMemoryID parses a string and validates the "mem" prefix.
func ParseMemoryID(s string) (ID, error) { return ParseWithPrefix(s, PrefixMemory) }

// ParseCheckpointID parses a string and validates the "acp" prefix.
func ParseCheckpointID(s string) (ID, error) { return ParseWithPrefix(s, PrefixCheckpoint) }

// ParseOrchestrationID parses a string and validates the "orch" prefix.
func ParseOrchestrationID(s string) (ID, error) { return ParseWithPrefix(s, PrefixOrchestration) }

// ParseSkillID parses a string and validates the "skl" prefix.
func ParseSkillID(s string) (ID, error) { return ParseWithPrefix(s, PrefixSkill) }

// ParseTraitID parses a string and validates the "trt" prefix.
func ParseTraitID(s string) (ID, error) { return ParseWithPrefix(s, PrefixTrait) }

// ParseBehaviorID parses a string and validates the "bhv" prefix.
func ParseBehaviorID(s string) (ID, error) { return ParseWithPrefix(s, PrefixBehavior) }

// ParsePersonaID parses a string and validates the "prs" prefix.
func ParsePersonaID(s string) (ID, error) { return ParseWithPrefix(s, PrefixPersona) }

// ParseAny parses a string into an ID without type checking the prefix.
func ParseAny(s string) (ID, error) { return Parse(s) }

// ──────────────────────────────────────────────────
// ID methods
// ──────────────────────────────────────────────────

// String returns the full TypeID string representation (prefix_suffix).
// Returns an empty string for the Nil ID.
func (i ID) String() string {
	if !i.valid {
		return ""
	}

	return i.inner.String()
}

// Prefix returns the prefix component of this ID.
func (i ID) Prefix() Prefix {
	if !i.valid {
		return ""
	}

	return Prefix(i.inner.Prefix())
}

// IsNil reports whether this ID is the zero value.
func (i ID) IsNil() bool {
	return !i.valid
}

// MarshalText implements encoding.TextMarshaler.
func (i ID) MarshalText() ([]byte, error) {
	if !i.valid {
		return []byte{}, nil
	}

	return []byte(i.inner.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (i *ID) UnmarshalText(data []byte) error {
	if len(data) == 0 {
		*i = Nil

		return nil
	}

	parsed, err := Parse(string(data))
	if err != nil {
		return err
	}

	*i = parsed

	return nil
}

// Value implements driver.Valuer for database storage.
// Returns nil for the Nil ID so that optional foreign key columns store NULL.
func (i ID) Value() (driver.Value, error) {
	if !i.valid {
		return nil, nil //nolint:nilnil // nil is the canonical NULL for driver.Valuer
	}

	return i.inner.String(), nil
}

// Scan implements sql.Scanner for database retrieval.
func (i *ID) Scan(src any) error {
	if src == nil {
		*i = Nil

		return nil
	}

	switch v := src.(type) {
	case string:
		if v == "" {
			*i = Nil

			return nil
		}

		return i.UnmarshalText([]byte(v))
	case []byte:
		if len(v) == 0 {
			*i = Nil

			return nil
		}

		return i.UnmarshalText(v)
	default:
		return fmt.Errorf("id: cannot scan %T into ID", src)
	}
}
