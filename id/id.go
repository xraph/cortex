// Package id provides TypeID-based identity types for all Cortex entities.
//
// Every entity in Cortex gets a type-prefixed, K-sortable, UUIDv7-based
// identifier. IDs are validated at parse time to ensure the prefix matches
// the expected type.
//
// Examples:
//
//	agt_01h2xcejqtf2nbrexx3vqjhp41
//	arun_01h2xcejqtf2nbrexx3vqjhp41
//	skl_01h455vb4pex5vsknk084sn02q
package id

import (
	"fmt"

	"go.jetify.com/typeid/v2"
)

// ──────────────────────────────────────────────────
// Prefix constants
// ──────────────────────────────────────────────────

const (
	PrefixAgent         = "agt"
	PrefixAgentRun      = "arun"
	PrefixTool          = "tool"
	PrefixToolCall      = "tcall"
	PrefixStep          = "astp"
	PrefixMemory        = "mem"
	PrefixCheckpoint    = "acp"
	PrefixOrchestration = "orch"
	PrefixSkill         = "skl"
	PrefixTrait         = "trt"
	PrefixBehavior      = "bhv"
	PrefixPersona       = "prs"
)

// ──────────────────────────────────────────────────
// Type aliases for readability
// ──────────────────────────────────────────────────

type AgentID = typeid.TypeID
type AgentRunID = typeid.TypeID
type ToolID = typeid.TypeID
type ToolCallID = typeid.TypeID
type StepID = typeid.TypeID
type MemoryID = typeid.TypeID
type CheckpointID = typeid.TypeID
type OrchestrationID = typeid.TypeID
type SkillID = typeid.TypeID
type TraitID = typeid.TypeID
type BehaviorID = typeid.TypeID
type PersonaID = typeid.TypeID

// AnyID is a TypeID that accepts any valid prefix.
type AnyID = typeid.TypeID

// ──────────────────────────────────────────────────
// Constructors
// ──────────────────────────────────────────────────

func NewAgentID() AgentID                 { return must(typeid.Generate(PrefixAgent)) }
func NewAgentRunID() AgentRunID           { return must(typeid.Generate(PrefixAgentRun)) }
func NewToolID() ToolID                   { return must(typeid.Generate(PrefixTool)) }
func NewToolCallID() ToolCallID           { return must(typeid.Generate(PrefixToolCall)) }
func NewStepID() StepID                   { return must(typeid.Generate(PrefixStep)) }
func NewMemoryID() MemoryID               { return must(typeid.Generate(PrefixMemory)) }
func NewCheckpointID() CheckpointID       { return must(typeid.Generate(PrefixCheckpoint)) }
func NewOrchestrationID() OrchestrationID { return must(typeid.Generate(PrefixOrchestration)) }
func NewSkillID() SkillID                 { return must(typeid.Generate(PrefixSkill)) }
func NewTraitID() TraitID                 { return must(typeid.Generate(PrefixTrait)) }
func NewBehaviorID() BehaviorID           { return must(typeid.Generate(PrefixBehavior)) }
func NewPersonaID() PersonaID             { return must(typeid.Generate(PrefixPersona)) }

// ──────────────────────────────────────────────────
// Parsing (validates prefix at parse time)
// ──────────────────────────────────────────────────

func ParseAgentID(s string) (AgentID, error)           { return parseWithPrefix(PrefixAgent, s) }
func ParseAgentRunID(s string) (AgentRunID, error)     { return parseWithPrefix(PrefixAgentRun, s) }
func ParseToolCallID(s string) (ToolCallID, error)     { return parseWithPrefix(PrefixToolCall, s) }
func ParseStepID(s string) (StepID, error)             { return parseWithPrefix(PrefixStep, s) }
func ParseMemoryID(s string) (MemoryID, error)         { return parseWithPrefix(PrefixMemory, s) }
func ParseCheckpointID(s string) (CheckpointID, error) { return parseWithPrefix(PrefixCheckpoint, s) }
func ParseOrchestrationID(s string) (OrchestrationID, error) {
	return parseWithPrefix(PrefixOrchestration, s)
}
func ParseSkillID(s string) (SkillID, error)       { return parseWithPrefix(PrefixSkill, s) }
func ParseTraitID(s string) (TraitID, error)       { return parseWithPrefix(PrefixTrait, s) }
func ParseBehaviorID(s string) (BehaviorID, error) { return parseWithPrefix(PrefixBehavior, s) }
func ParsePersonaID(s string) (PersonaID, error)   { return parseWithPrefix(PrefixPersona, s) }
func ParseAny(s string) (AnyID, error)             { return typeid.Parse(s) }

// ──────────────────────────────────────────────────
// Helpers
// ──────────────────────────────────────────────────

func parseWithPrefix(expected, s string) (typeid.TypeID, error) {
	tid, err := typeid.Parse(s)
	if err != nil {
		return tid, err
	}
	if tid.Prefix() != expected {
		return tid, fmt.Errorf("id: expected prefix %q, got %q", expected, tid.Prefix())
	}
	return tid, nil
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
