package mongo

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/xraph/grove"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/communication"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/perception"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/trait"
)

// ──────────────────────────────────────────────────
// Agent model
// ──────────────────────────────────────────────────

type agentModel struct {
	grove.BaseModel `grove:"table:cortex_agents"`
	ID              string            `grove:"id,pk"              bson:"_id"`
	Name            string            `grove:"name"               bson:"name"`
	Description     string            `grove:"description"        bson:"description"`
	AppID           string            `grove:"app_id"             bson:"app_id"`
	SystemPrompt    string            `grove:"system_prompt"      bson:"system_prompt"`
	Model           string            `grove:"model"              bson:"model"`
	Tools           []string          `grove:"tools"              bson:"tools,omitempty"`
	MaxSteps        int               `grove:"max_steps"          bson:"max_steps"`
	MaxTokens       int               `grove:"max_tokens"         bson:"max_tokens"`
	Temperature     float64           `grove:"temperature"        bson:"temperature"`
	ReasoningLoop   string            `grove:"reasoning_loop"     bson:"reasoning_loop"`
	Guardrails      map[string]any    `grove:"guardrails"         bson:"guardrails,omitempty"`
	Metadata        map[string]any    `grove:"metadata"           bson:"metadata,omitempty"`
	Enabled         bool              `grove:"enabled"            bson:"enabled"`
	PersonaRef      string            `grove:"persona_ref"        bson:"persona_ref"`
	InlineSkills    []string          `grove:"inline_skills"      bson:"inline_skills,omitempty"`
	InlineTraits    []string          `grove:"inline_traits"      bson:"inline_traits,omitempty"`
	InlineBehaviors []string          `grove:"inline_behaviors"   bson:"inline_behaviors,omitempty"`
	Sections        []prompt.Section  `grove:"sections"           bson:"sections,omitempty"`
	ScopeL0         string            `grove:"scope_l0"       bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"       bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"       bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"    bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"    bson:"scope_canon"`
	CreatedAt       time.Time         `grove:"created_at"         bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"         bson:"updated_at"`
}

func agentToModel(c *agent.Config) *agentModel {
	l0, l1, l2, extra := scopeColumns(c.Scope)
	return &agentModel{
		ID:          c.ID.String(),
		Name:        c.Name,
		Description: c.Description,
		// app_id is a vestigial field: the agent surface lost AppID this
		// phase (the (scope_canon, name) unique index replaced
		// (app_id, name)), but the field itself isn't dropped from the
		// document here, so every write leaves it empty rather than
		// reading a field that no longer exists on Config.
		AppID:           "",
		SystemPrompt:    c.SystemPrompt,
		Model:           c.Model,
		Tools:           c.Tools,
		MaxSteps:        c.MaxSteps,
		MaxTokens:       c.MaxTokens,
		Temperature:     c.Temperature,
		ReasoningLoop:   c.ReasoningLoop,
		Guardrails:      c.Guardrails,
		Metadata:        c.Metadata,
		Enabled:         c.Enabled,
		PersonaRef:      c.PersonaRef,
		InlineSkills:    c.InlineSkills,
		InlineTraits:    c.InlineTraits,
		InlineBehaviors: c.InlineBehaviors,
		Sections:        c.Sections,
		ScopeL0:         l0,
		ScopeL1:         l1,
		ScopeL2:         l2,
		ScopeExtra:      extra,
		ScopeCanon:      c.Scope.Canonical(),
		CreatedAt:       c.CreatedAt,
		UpdatedAt:       c.UpdatedAt,
	}
}

func agentFromModel(m *agentModel) (*agent.Config, error) {
	agentID, err := id.ParseAgentID(m.ID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("agent %s: %w", agentID, err)
	}
	return &agent.Config{
		Entity:          cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:              agentID,
		Name:            m.Name,
		Description:     m.Description,
		Scope:           scope,
		SystemPrompt:    m.SystemPrompt,
		Model:           m.Model,
		Tools:           m.Tools,
		MaxSteps:        m.MaxSteps,
		MaxTokens:       m.MaxTokens,
		Temperature:     m.Temperature,
		ReasoningLoop:   m.ReasoningLoop,
		Guardrails:      m.Guardrails,
		Metadata:        m.Metadata,
		Enabled:         m.Enabled,
		PersonaRef:      m.PersonaRef,
		InlineSkills:    m.InlineSkills,
		InlineTraits:    m.InlineTraits,
		InlineBehaviors: m.InlineBehaviors,
		Sections:        m.Sections,
	}, nil
}

// ──────────────────────────────────────────────────
// Run model
// ──────────────────────────────────────────────────

type runModel struct {
	grove.BaseModel `grove:"table:cortex_runs"`
	ID              string            `grove:"id,pk"          bson:"_id"`
	AgentID         string            `grove:"agent_id"       bson:"agent_id"`
	SessionID       string            `grove:"session_id"     bson:"session_id"`
	State           string            `grove:"state"          bson:"state"`
	Input           string            `grove:"input"          bson:"input"`
	Output          string            `grove:"output"         bson:"output"`
	Error           string            `grove:"error"          bson:"error"`
	StepCount       int               `grove:"step_count"     bson:"step_count"`
	TokensUsed      int               `grove:"tokens_used"    bson:"tokens_used"`
	StartedAt       *time.Time        `grove:"started_at"     bson:"started_at,omitempty"`
	CompletedAt     *time.Time        `grove:"completed_at"   bson:"completed_at,omitempty"`
	PersonaRef      string            `grove:"persona_ref"    bson:"persona_ref"`
	Metadata        map[string]any    `grove:"metadata"       bson:"metadata,omitempty"`
	ScopeL0         string            `grove:"scope_l0"       bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"       bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"       bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"    bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"    bson:"scope_canon"`
	CreatedAt       time.Time         `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"     bson:"updated_at"`
}

func runToModel(r *run.Run) *runModel {
	l0, l1, l2, extra := scopeColumns(r.Scope)
	return &runModel{
		ID:          r.ID.String(),
		AgentID:     r.AgentID.String(),
		SessionID:   r.SessionID.String(),
		State:       string(r.State),
		Input:       r.Input,
		Output:      r.Output,
		Error:       r.Error,
		StepCount:   r.StepCount,
		TokensUsed:  r.TokensUsed,
		StartedAt:   r.StartedAt,
		CompletedAt: r.CompletedAt,
		PersonaRef:  r.PersonaRef,
		Metadata:    r.Metadata,
		ScopeL0:     l0,
		ScopeL1:     l1,
		ScopeL2:     l2,
		ScopeExtra:  extra,
		ScopeCanon:  r.Scope.Canonical(),
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

func runFromModel(m *runModel) (*run.Run, error) {
	runID, err := id.ParseAgentRunID(m.ID)
	if err != nil {
		return nil, err
	}
	agentID, err := id.ParseAgentID(m.AgentID)
	if err != nil {
		return nil, err
	}
	sessionID, err := id.ParseOptionalSessionID(m.SessionID)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", runID, err)
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("run %s: %w", runID, err)
	}
	return &run.Run{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          runID,
		AgentID:     agentID,
		SessionID:   sessionID,
		Scope:       scope,
		State:       run.State(m.State),
		Input:       m.Input,
		Output:      m.Output,
		Error:       m.Error,
		StepCount:   m.StepCount,
		TokensUsed:  m.TokensUsed,
		StartedAt:   m.StartedAt,
		CompletedAt: m.CompletedAt,
		PersonaRef:  m.PersonaRef,
		Metadata:    m.Metadata,
	}, nil
}

// ──────────────────────────────────────────────────
// Step model
// ──────────────────────────────────────────────────

type stepModel struct {
	grove.BaseModel `grove:"table:cortex_steps"`
	ID              string            `grove:"id,pk"          bson:"_id"`
	RunID           string            `grove:"run_id"         bson:"run_id"`
	Index           int               `grove:"index"          bson:"index"`
	Type            string            `grove:"type"           bson:"type"`
	Input           string            `grove:"input"          bson:"input"`
	Output          string            `grove:"output"         bson:"output"`
	TokensUsed      int               `grove:"tokens_used"    bson:"tokens_used"`
	StartedAt       *time.Time        `grove:"started_at"     bson:"started_at,omitempty"`
	CompletedAt     *time.Time        `grove:"completed_at"   bson:"completed_at,omitempty"`
	Metadata        map[string]any    `grove:"metadata"       bson:"metadata,omitempty"`
	ScopeL0         string            `grove:"scope_l0"       bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"       bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"       bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"    bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"    bson:"scope_canon"`
	CreatedAt       time.Time         `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"     bson:"updated_at"`
}

func stepToModel(s *run.Step) *stepModel {
	l0, l1, l2, extra := scopeColumns(s.Scope)
	return &stepModel{
		ID:          s.ID.String(),
		RunID:       s.RunID.String(),
		Index:       s.Index,
		Type:        s.Type,
		Input:       s.Input,
		Output:      s.Output,
		TokensUsed:  s.TokensUsed,
		StartedAt:   s.StartedAt,
		CompletedAt: s.CompletedAt,
		Metadata:    s.Metadata,
		ScopeL0:     l0,
		ScopeL1:     l1,
		ScopeL2:     l2,
		ScopeExtra:  extra,
		ScopeCanon:  s.Scope.Canonical(),
		CreatedAt:   s.CreatedAt,
		UpdatedAt:   s.UpdatedAt,
	}
}

func stepFromModel(m *stepModel) (*run.Step, error) {
	stepID, err := id.ParseStepID(m.ID)
	if err != nil {
		return nil, err
	}
	runID, err := id.ParseAgentRunID(m.RunID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("step %s: %w", stepID, err)
	}
	return &run.Step{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          stepID,
		RunID:       runID,
		Scope:       scope,
		Index:       m.Index,
		Type:        m.Type,
		Input:       m.Input,
		Output:      m.Output,
		TokensUsed:  m.TokensUsed,
		StartedAt:   m.StartedAt,
		CompletedAt: m.CompletedAt,
		Metadata:    m.Metadata,
	}, nil
}

// ──────────────────────────────────────────────────
// ToolCall model
// ──────────────────────────────────────────────────

type toolCallModel struct {
	grove.BaseModel `grove:"table:cortex_tool_calls"`
	ID              string            `grove:"id,pk"          bson:"_id"`
	StepID          string            `grove:"step_id"        bson:"step_id"`
	RunID           string            `grove:"run_id"         bson:"run_id"`
	ToolName        string            `grove:"tool_name"      bson:"tool_name"`
	Arguments       string            `grove:"arguments"      bson:"arguments"`
	Result          string            `grove:"result"         bson:"result"`
	Error           string            `grove:"error"          bson:"error"`
	StartedAt       *time.Time        `grove:"started_at"     bson:"started_at,omitempty"`
	CompletedAt     *time.Time        `grove:"completed_at"   bson:"completed_at,omitempty"`
	Metadata        map[string]any    `grove:"metadata"       bson:"metadata,omitempty"`
	ScopeL0         string            `grove:"scope_l0"       bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"       bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"       bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"    bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"    bson:"scope_canon"`
	CreatedAt       time.Time         `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"     bson:"updated_at"`
}

func toolCallToModel(tc *run.ToolCall) *toolCallModel {
	l0, l1, l2, extra := scopeColumns(tc.Scope)
	return &toolCallModel{
		ID:          tc.ID.String(),
		StepID:      tc.StepID.String(),
		RunID:       tc.RunID.String(),
		ToolName:    tc.ToolName,
		Arguments:   tc.Arguments,
		Result:      tc.Result,
		Error:       tc.Error,
		StartedAt:   tc.StartedAt,
		CompletedAt: tc.CompletedAt,
		Metadata:    tc.Metadata,
		ScopeL0:     l0,
		ScopeL1:     l1,
		ScopeL2:     l2,
		ScopeExtra:  extra,
		ScopeCanon:  tc.Scope.Canonical(),
		CreatedAt:   tc.CreatedAt,
		UpdatedAt:   tc.UpdatedAt,
	}
}

func toolCallFromModel(m *toolCallModel) (*run.ToolCall, error) {
	tcID, err := id.ParseToolCallID(m.ID)
	if err != nil {
		return nil, err
	}
	stepID, err := id.ParseStepID(m.StepID)
	if err != nil {
		return nil, err
	}
	runID, err := id.ParseAgentRunID(m.RunID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("tool call %s: %w", tcID, err)
	}
	return &run.ToolCall{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          tcID,
		StepID:      stepID,
		RunID:       runID,
		Scope:       scope,
		ToolName:    m.ToolName,
		Arguments:   m.Arguments,
		Result:      m.Result,
		Error:       m.Error,
		StartedAt:   m.StartedAt,
		CompletedAt: m.CompletedAt,
		Metadata:    m.Metadata,
	}, nil
}

// ──────────────────────────────────────────────────
// Memory model
// ──────────────────────────────────────────────────

type memoryModel struct {
	grove.BaseModel `grove:"table:cortex_memories"`
	ID              string            `grove:"id,pk"          bson:"_id,omitempty"`
	AgentID         string            `grove:"agent_id"       bson:"agent_id"`
	SessionID       string            `grove:"session_id"     bson:"session_id"`
	Kind            string            `grove:"kind"           bson:"kind"`
	Key             string            `grove:"key"            bson:"key"`
	Content         string            `grove:"content"        bson:"content"`
	Metadata        map[string]any    `grove:"metadata"       bson:"metadata,omitempty"`
	ScopeL0         string            `grove:"scope_l0"       bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"       bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"       bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"    bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"    bson:"scope_canon"`
	CreatedAt       time.Time         `grove:"created_at"     bson:"created_at"`
}

// ──────────────────────────────────────────────────
// Checkpoint model
// ──────────────────────────────────────────────────

type checkpointModel struct {
	grove.BaseModel `grove:"table:cortex_checkpoints"`
	ID              string               `grove:"id,pk"          bson:"_id"`
	RunID           string               `grove:"run_id"         bson:"run_id"`
	AgentID         string               `grove:"agent_id"       bson:"agent_id"`
	Reason          string               `grove:"reason"         bson:"reason"`
	StepIndex       int                  `grove:"step_index"     bson:"step_index"`
	State           string               `grove:"state"          bson:"state"`
	Decision        *checkpoint.Decision `grove:"decision"       bson:"decision,omitempty"`
	Metadata        map[string]any       `grove:"metadata"       bson:"metadata,omitempty"`
	ScopeL0         string               `grove:"scope_l0"       bson:"scope_l0"`
	ScopeL1         string               `grove:"scope_l1"       bson:"scope_l1"`
	ScopeL2         string               `grove:"scope_l2"       bson:"scope_l2"`
	ScopeExtra      map[string]string    `grove:"scope_extra"    bson:"scope_extra,omitempty"`
	ScopeCanon      string               `grove:"scope_canon"    bson:"scope_canon"`
	CreatedAt       time.Time            `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time            `grove:"updated_at"     bson:"updated_at"`
}

func checkpointToModel(cp *checkpoint.Checkpoint) *checkpointModel {
	l0, l1, l2, extra := scopeColumns(cp.Scope)
	return &checkpointModel{
		ID:         cp.ID.String(),
		RunID:      cp.RunID.String(),
		AgentID:    cp.AgentID.String(),
		Reason:     cp.Reason,
		StepIndex:  cp.StepIndex,
		State:      cp.State,
		Decision:   cp.Decision,
		Metadata:   cp.Metadata,
		ScopeL0:    l0,
		ScopeL1:    l1,
		ScopeL2:    l2,
		ScopeExtra: extra,
		ScopeCanon: cp.Scope.Canonical(),
		CreatedAt:  cp.CreatedAt,
		UpdatedAt:  cp.UpdatedAt,
	}
}

func checkpointFromModel(m *checkpointModel) (*checkpoint.Checkpoint, error) {
	cpID, err := id.ParseCheckpointID(m.ID)
	if err != nil {
		return nil, err
	}
	runID, err := id.ParseAgentRunID(m.RunID)
	if err != nil {
		return nil, err
	}
	agentID, err := id.ParseAgentID(m.AgentID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("checkpoint %s: %w", cpID, err)
	}
	return &checkpoint.Checkpoint{
		Entity:    cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:        cpID,
		RunID:     runID,
		AgentID:   agentID,
		Scope:     scope,
		Reason:    m.Reason,
		StepIndex: m.StepIndex,
		State:     m.State,
		Decision:  m.Decision,
		Metadata:  m.Metadata,
	}, nil
}

// ──────────────────────────────────────────────────
// Skill model
// ──────────────────────────────────────────────────

type skillModel struct {
	grove.BaseModel      `grove:"table:cortex_skills"`
	ID                   string               `grove:"id,pk"                  bson:"_id"`
	Name                 string               `grove:"name"                   bson:"name"`
	Description          string               `grove:"description"            bson:"description"`
	AppID                string               `grove:"app_id"                 bson:"app_id"`
	Tools                []skill.ToolBinding  `grove:"tools"                 bson:"tools,omitempty"`
	Knowledge            []skill.KnowledgeRef `grove:"knowledge"             bson:"knowledge,omitempty"`
	SystemPromptFragment string               `grove:"system_prompt_fragment" bson:"system_prompt_fragment"`
	Dependencies         []string             `grove:"dependencies"           bson:"dependencies,omitempty"`
	DefaultProficiency   string               `grove:"default_proficiency"    bson:"default_proficiency"`
	Metadata             map[string]any       `grove:"metadata"               bson:"metadata,omitempty"`
	ScopeL0              string               `grove:"scope_l0"       bson:"scope_l0"`
	ScopeL1              string               `grove:"scope_l1"       bson:"scope_l1"`
	ScopeL2              string               `grove:"scope_l2"       bson:"scope_l2"`
	ScopeExtra           map[string]string    `grove:"scope_extra"    bson:"scope_extra,omitempty"`
	ScopeCanon           string               `grove:"scope_canon"    bson:"scope_canon"`
	CreatedAt            time.Time            `grove:"created_at"             bson:"created_at"`
	UpdatedAt            time.Time            `grove:"updated_at"             bson:"updated_at"`
}

func skillToModel(s *skill.Skill) *skillModel {
	l0, l1, l2, extra := scopeColumns(s.Scope)
	return &skillModel{
		ID:          s.ID.String(),
		Name:        s.Name,
		Description: s.Description,
		// app_id is a vestigial field: the skill surface lost AppID this
		// phase (the (scope_canon, name) unique index replaced
		// (app_id, name)), but the field itself isn't dropped from the
		// document here, so every write leaves it empty rather than
		// reading a field that no longer exists on Skill.
		AppID:                "",
		Tools:                s.Tools,
		Knowledge:            s.Knowledge,
		SystemPromptFragment: s.SystemPromptFragment,
		Dependencies:         s.Dependencies,
		DefaultProficiency:   string(s.DefaultProficiency),
		Metadata:             s.Metadata,
		ScopeL0:              l0,
		ScopeL1:              l1,
		ScopeL2:              l2,
		ScopeExtra:           extra,
		ScopeCanon:           s.Scope.Canonical(),
		CreatedAt:            s.CreatedAt,
		UpdatedAt:            s.UpdatedAt,
	}
}

func skillFromModel(m *skillModel) (*skill.Skill, error) {
	skillID, err := id.ParseSkillID(m.ID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("skill %s: %w", skillID, err)
	}
	return &skill.Skill{
		Entity:               cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:                   skillID,
		Name:                 m.Name,
		Description:          m.Description,
		Scope:                scope,
		Tools:                m.Tools,
		Knowledge:            m.Knowledge,
		SystemPromptFragment: m.SystemPromptFragment,
		Dependencies:         m.Dependencies,
		DefaultProficiency:   skill.Proficiency(m.DefaultProficiency),
		Metadata:             m.Metadata,
	}, nil
}

// ──────────────────────────────────────────────────
// Trait model
// ──────────────────────────────────────────────────

type traitModel struct {
	grove.BaseModel `grove:"table:cortex_traits"`
	ID              string            `grove:"id,pk"          bson:"_id"`
	Name            string            `grove:"name"           bson:"name"`
	Description     string            `grove:"description"    bson:"description"`
	AppID           string            `grove:"app_id"         bson:"app_id"`
	Dimensions      []trait.Dimension `grove:"dimensions"     bson:"dimensions,omitempty"`
	Influences      []trait.Influence `grove:"influences"     bson:"influences,omitempty"`
	Category        string            `grove:"category"       bson:"category"`
	Metadata        map[string]any    `grove:"metadata"       bson:"metadata,omitempty"`
	ScopeL0         string            `grove:"scope_l0"       bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"       bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"       bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"    bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"    bson:"scope_canon"`
	CreatedAt       time.Time         `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"     bson:"updated_at"`
}

func traitToModel(t *trait.Trait) *traitModel {
	l0, l1, l2, extra := scopeColumns(t.Scope)
	return &traitModel{
		ID:          t.ID.String(),
		Name:        t.Name,
		Description: t.Description,
		// app_id is a vestigial field: the trait surface lost AppID this
		// phase (the (scope_canon, name) unique index replaced
		// (app_id, name)), but the field itself isn't dropped from the
		// document here, so every write leaves it empty rather than
		// reading a field that no longer exists on Trait.
		AppID:      "",
		Dimensions: t.Dimensions,
		Influences: t.Influences,
		Category:   string(t.Category),
		Metadata:   t.Metadata,
		ScopeL0:    l0,
		ScopeL1:    l1,
		ScopeL2:    l2,
		ScopeExtra: extra,
		ScopeCanon: t.Scope.Canonical(),
		CreatedAt:  t.CreatedAt,
		UpdatedAt:  t.UpdatedAt,
	}
}

func traitFromModel(m *traitModel) (*trait.Trait, error) {
	traitID, err := id.ParseTraitID(m.ID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("trait %s: %w", traitID, err)
	}
	return &trait.Trait{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          traitID,
		Name:        m.Name,
		Description: m.Description,
		Scope:       scope,
		Dimensions:  m.Dimensions,
		Influences:  m.Influences,
		Category:    trait.Category(m.Category),
		Metadata:    m.Metadata,
	}, nil
}

// ──────────────────────────────────────────────────
// Behavior model
// ──────────────────────────────────────────────────

type behaviorModel struct {
	grove.BaseModel `grove:"table:cortex_behaviors"`
	ID              string             `grove:"id,pk"          bson:"_id"`
	Name            string             `grove:"name"           bson:"name"`
	Description     string             `grove:"description"    bson:"description"`
	AppID           string             `grove:"app_id"         bson:"app_id"`
	Triggers        []behavior.Trigger `grove:"triggers"       bson:"triggers,omitempty"`
	Actions         []behavior.Action  `grove:"actions"        bson:"actions,omitempty"`
	Priority        int                `grove:"priority"       bson:"priority"`
	RequiresSkill   string             `grove:"requires_skill" bson:"requires_skill"`
	RequiresTrait   string             `grove:"requires_trait" bson:"requires_trait"`
	Metadata        map[string]any     `grove:"metadata"       bson:"metadata,omitempty"`
	ScopeL0         string             `grove:"scope_l0"       bson:"scope_l0"`
	ScopeL1         string             `grove:"scope_l1"       bson:"scope_l1"`
	ScopeL2         string             `grove:"scope_l2"       bson:"scope_l2"`
	ScopeExtra      map[string]string  `grove:"scope_extra"    bson:"scope_extra,omitempty"`
	ScopeCanon      string             `grove:"scope_canon"    bson:"scope_canon"`
	CreatedAt       time.Time          `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time          `grove:"updated_at"     bson:"updated_at"`
}

func behaviorToModel(b *behavior.Behavior) *behaviorModel {
	l0, l1, l2, extra := scopeColumns(b.Scope)
	return &behaviorModel{
		ID:          b.ID.String(),
		Name:        b.Name,
		Description: b.Description,
		// app_id is a vestigial field: the behavior surface lost AppID
		// this phase (the (scope_canon, name) unique index replaced
		// (app_id, name)), but the field itself isn't dropped from the
		// document here, so every write leaves it empty rather than
		// reading a field that no longer exists on Behavior.
		AppID:         "",
		Triggers:      b.Triggers,
		Actions:       b.Actions,
		Priority:      b.Priority,
		RequiresSkill: b.RequiresSkill,
		RequiresTrait: b.RequiresTrait,
		Metadata:      b.Metadata,
		ScopeL0:       l0,
		ScopeL1:       l1,
		ScopeL2:       l2,
		ScopeExtra:    extra,
		ScopeCanon:    b.Scope.Canonical(),
		CreatedAt:     b.CreatedAt,
		UpdatedAt:     b.UpdatedAt,
	}
}

func behaviorFromModel(m *behaviorModel) (*behavior.Behavior, error) {
	behaviorID, err := id.ParseBehaviorID(m.ID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("behavior %s: %w", behaviorID, err)
	}
	return &behavior.Behavior{
		Entity:        cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:            behaviorID,
		Name:          m.Name,
		Description:   m.Description,
		Scope:         scope,
		Triggers:      m.Triggers,
		Actions:       m.Actions,
		Priority:      m.Priority,
		RequiresSkill: m.RequiresSkill,
		RequiresTrait: m.RequiresTrait,
		Metadata:      m.Metadata,
	}, nil
}

// ──────────────────────────────────────────────────
// Persona model
// ──────────────────────────────────────────────────

type personaModel struct {
	grove.BaseModel    `grove:"table:cortex_personas"`
	ID                 string                    `grove:"id,pk"                bson:"_id"`
	Name               string                    `grove:"name"                 bson:"name"`
	Description        string                    `grove:"description"          bson:"description"`
	AppID              string                    `grove:"app_id"               bson:"app_id"`
	Identity           string                    `grove:"identity"             bson:"identity"`
	Skills             []persona.SkillAssignment `grove:"skills"               bson:"skills,omitempty"`
	Traits             []persona.TraitAssignment `grove:"traits"               bson:"traits,omitempty"`
	Behaviors          []string                  `grove:"behaviors"            bson:"behaviors,omitempty"`
	CognitiveStyle     cognitive.Style           `grove:"cognitive_style"      bson:"cognitive_style,omitempty"`
	CommunicationStyle communication.Style       `grove:"communication_style"  bson:"communication_style,omitempty"`
	Perception         perception.Model          `grove:"perception"           bson:"perception,omitempty"`
	Metadata           map[string]any            `grove:"metadata"             bson:"metadata,omitempty"`
	ScopeL0            string                    `grove:"scope_l0"       bson:"scope_l0"`
	ScopeL1            string                    `grove:"scope_l1"       bson:"scope_l1"`
	ScopeL2            string                    `grove:"scope_l2"       bson:"scope_l2"`
	ScopeExtra         map[string]string         `grove:"scope_extra"    bson:"scope_extra,omitempty"`
	ScopeCanon         string                    `grove:"scope_canon"    bson:"scope_canon"`
	CreatedAt          time.Time                 `grove:"created_at"           bson:"created_at"`
	UpdatedAt          time.Time                 `grove:"updated_at"           bson:"updated_at"`
}

func personaToModel(p *persona.Persona) *personaModel {
	l0, l1, l2, extra := scopeColumns(p.Scope)
	return &personaModel{
		ID:          p.ID.String(),
		Name:        p.Name,
		Description: p.Description,
		// app_id is a vestigial field: the persona surface lost AppID
		// this phase (the (scope_canon, name) unique index replaced
		// (app_id, name)), but the field itself isn't dropped from the
		// document here, so every write leaves it empty rather than
		// reading a field that no longer exists on Persona.
		AppID:              "",
		Identity:           p.Identity,
		Skills:             p.Skills,
		Traits:             p.Traits,
		Behaviors:          p.Behaviors,
		CognitiveStyle:     p.CognitiveStyle,
		CommunicationStyle: p.CommunicationStyle,
		Perception:         p.Perception,
		Metadata:           p.Metadata,
		ScopeL0:            l0,
		ScopeL1:            l1,
		ScopeL2:            l2,
		ScopeExtra:         extra,
		ScopeCanon:         p.Scope.Canonical(),
		CreatedAt:          p.CreatedAt,
		UpdatedAt:          p.UpdatedAt,
	}
}

func personaFromModel(m *personaModel) (*persona.Persona, error) {
	personaID, err := id.ParsePersonaID(m.ID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("persona %s: %w", personaID, err)
	}
	return &persona.Persona{
		Entity:             cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:                 personaID,
		Name:               m.Name,
		Description:        m.Description,
		Scope:              scope,
		Identity:           m.Identity,
		Skills:             m.Skills,
		Traits:             m.Traits,
		Behaviors:          m.Behaviors,
		CognitiveStyle:     m.CognitiveStyle,
		CommunicationStyle: m.CommunicationStyle,
		Perception:         m.Perception,
		Metadata:           m.Metadata,
	}, nil
}

// ──────────────────────────────────────────────────
// Session model
// ──────────────────────────────────────────────────

type sessionModel struct {
	grove.BaseModel `grove:"table:cortex_sessions"`
	ID              string            `grove:"id,pk"            bson:"_id"`
	AgentID         string            `grove:"agent_id"         bson:"agent_id"`
	Title           string            `grove:"title"            bson:"title"`
	MessageCount    int               `grove:"message_count"    bson:"message_count"`
	LastMessage     string            `grove:"last_message"     bson:"last_message"`
	IsDefault       bool              `grove:"is_default"       bson:"is_default"`
	Metadata        map[string]any    `grove:"metadata"         bson:"metadata,omitempty"`
	BackfilledBy    string            `grove:"backfilled_by"    bson:"backfilled_by,omitempty"`
	ScopeL0         string            `grove:"scope_l0"         bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"         bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"         bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"      bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"      bson:"scope_canon"`
	CreatedAt       time.Time         `grove:"created_at"       bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"       bson:"updated_at"`
}

func sessionToModel(sess *session.Session) *sessionModel {
	l0, l1, l2, extra := scopeColumns(sess.Scope)
	return &sessionModel{
		ID:           sess.ID.String(),
		AgentID:      sess.AgentID.String(),
		Title:        sess.Title,
		MessageCount: sess.MessageCount,
		LastMessage:  sess.LastMessage,
		IsDefault:    sess.IsDefault,
		Metadata:     sess.Metadata,
		BackfilledBy: sess.BackfilledBy,
		ScopeL0:      l0,
		ScopeL1:      l1,
		ScopeL2:      l2,
		ScopeExtra:   extra,
		ScopeCanon:   sess.Scope.Canonical(),
		CreatedAt:    sess.CreatedAt,
		UpdatedAt:    sess.UpdatedAt,
	}
}

func sessionFromModel(m *sessionModel) (*session.Session, error) {
	sessionID, err := id.ParseSessionID(m.ID)
	if err != nil {
		return nil, err
	}
	agentID, err := id.ParseAgentID(m.AgentID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("session %s: %w", sessionID, err)
	}
	return &session.Session{
		Entity:       cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:           sessionID,
		AgentID:      agentID,
		Scope:        scope,
		Title:        m.Title,
		MessageCount: m.MessageCount,
		LastMessage:  m.LastMessage,
		IsDefault:    m.IsDefault,
		Metadata:     m.Metadata,
		BackfilledBy: m.BackfilledBy,
	}, nil
}

// ──────────────────────────────────────────────────
// Config model
// ──────────────────────────────────────────────────

type orchestrationConfigModel struct {
	grove.BaseModel `grove:"table:cortex_orchestration_configs"`
	ID              string                      `grove:"id,pk"        bson:"_id"`
	Name            string                      `grove:"name"         bson:"name"`
	Description     string                      `grove:"description"  bson:"description"`
	AppID           string                      `grove:"app_id"       bson:"app_id"`
	Strategy        string                      `grove:"strategy"     bson:"strategy"`
	Participants    []orchestration.Participant `grove:"participants" bson:"participants,omitempty"`
	Settings        orchestration.Settings      `grove:"settings"     bson:"settings,omitempty"`
	Metadata        map[string]any              `grove:"metadata"     bson:"metadata,omitempty"`
	ScopeL0         string                      `grove:"scope_l0"     bson:"scope_l0"`
	ScopeL1         string                      `grove:"scope_l1"     bson:"scope_l1"`
	ScopeL2         string                      `grove:"scope_l2"     bson:"scope_l2"`
	ScopeExtra      map[string]string           `grove:"scope_extra"  bson:"scope_extra,omitempty"`
	ScopeCanon      string                      `grove:"scope_canon"  bson:"scope_canon"`
	CreatedAt       time.Time                   `grove:"created_at"   bson:"created_at"`
	UpdatedAt       time.Time                   `grove:"updated_at"   bson:"updated_at"`
}

func orchestrationConfigToModel(c *orchestration.Config) *orchestrationConfigModel {
	l0, l1, l2, extra := scopeColumns(c.Scope)
	return &orchestrationConfigModel{
		ID:          c.ID.String(),
		Name:        c.Name,
		Description: c.Description,
		// app_id is a vestigial field: orchestration.Config dropped
		// AppID this round (it never governed anything once the unique
		// index went scope-keyed — GetOrchestrationByName's appID
		// predicate could only ever turn a hit into a miss, never
		// disambiguate two rows), but the field itself isn't dropped
		// from the document here, so every write leaves it empty rather
		// than reading a field that no longer exists on Config.
		AppID:        "",
		Strategy:     c.Strategy,
		Participants: c.Participants,
		Settings:     c.Settings,
		Metadata:     c.Metadata,
		ScopeL0:      l0,
		ScopeL1:      l1,
		ScopeL2:      l2,
		ScopeExtra:   extra,
		ScopeCanon:   c.Scope.Canonical(),
		CreatedAt:    c.CreatedAt,
		UpdatedAt:    c.UpdatedAt,
	}
}

func orchestrationConfigFromModel(m *orchestrationConfigModel) (*orchestration.Config, error) {
	cfgID, err := id.ParseOrchestrationConfigID(m.ID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("orchestration config %s: %w", cfgID, err)
	}
	return &orchestration.Config{
		Entity:       cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:           cfgID,
		Name:         m.Name,
		Description:  m.Description,
		Scope:        scope,
		Strategy:     m.Strategy,
		Participants: m.Participants,
		Settings:     m.Settings,
		Metadata:     m.Metadata,
	}, nil
}

// ──────────────────────────────────────────────────
// Run model
// ──────────────────────────────────────────────────

type orchestrationRunModel struct {
	grove.BaseModel `grove:"table:cortex_orchestration_runs"`
	ID              string            `grove:"id,pk"         bson:"_id"`
	ConfigID        string            `grove:"config_id"     bson:"config_id"`
	AppID           string            `grove:"app_id"        bson:"app_id"`
	Strategy        string            `grove:"strategy"      bson:"strategy"`
	Status          string            `grove:"status"        bson:"status"`
	Input           string            `grove:"input"         bson:"input"`
	Output          string            `grove:"output"        bson:"output"`
	Error           string            `grove:"error"         bson:"error"`
	AgentRunIDs     []string          `grove:"agent_run_ids" bson:"agent_run_ids,omitempty"`
	ScopeL0         string            `grove:"scope_l0"      bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"      bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"      bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"   bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"   bson:"scope_canon"`
	StartedAt       time.Time         `grove:"started_at"    bson:"started_at"`
	CompletedAt     *time.Time        `grove:"completed_at"  bson:"completed_at,omitempty"`
	CreatedAt       time.Time         `grove:"created_at"    bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"    bson:"updated_at"`
}

func orchestrationRunToModel(r *orchestration.Run) *orchestrationRunModel {
	runIDs := make([]string, len(r.AgentRunIDs))
	for i, rid := range r.AgentRunIDs {
		runIDs[i] = rid.String()
	}
	l0, l1, l2, extra := scopeColumns(r.Scope)
	return &orchestrationRunModel{
		ID:       r.ID.String(),
		ConfigID: r.ConfigID.String(),
		// app_id is vestigial for the same reason as
		// orchestrationConfigToModel's: orchestration.Run dropped AppID
		// this round, and the field stays empty rather than reading a
		// field that no longer exists.
		AppID:       "",
		Strategy:    r.Strategy,
		Status:      r.Status,
		Input:       r.Input,
		Output:      r.Output,
		Error:       r.Error,
		AgentRunIDs: runIDs,
		ScopeL0:     l0,
		ScopeL1:     l1,
		ScopeL2:     l2,
		ScopeExtra:  extra,
		ScopeCanon:  r.Scope.Canonical(),
		StartedAt:   r.StartedAt,
		CompletedAt: r.CompletedAt,
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

func orchestrationRunFromModel(m *orchestrationRunModel) (*orchestration.Run, error) {
	runID, err := id.ParseOrchestrationID(m.ID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("orchestration run %s: %w", runID, err)
	}
	r := &orchestration.Run{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          runID,
		Scope:       scope,
		Strategy:    m.Strategy,
		Status:      m.Status,
		Input:       m.Input,
		Output:      m.Output,
		Error:       m.Error,
		StartedAt:   m.StartedAt,
		CompletedAt: m.CompletedAt,
	}
	if m.ConfigID != "" {
		cfgID, cerr := id.ParseOrchestrationConfigID(m.ConfigID)
		if cerr != nil {
			return nil, cerr
		}
		r.ConfigID = cfgID
	}
	for _, s := range m.AgentRunIDs {
		rid, perr := id.ParseAgentRunID(s)
		if perr != nil {
			return nil, perr
		}
		r.AgentRunIDs = append(r.AgentRunIDs, rid)
	}
	return r, nil
}

// ──────────────────────────────────────────────────
// JSON helper
// ──────────────────────────────────────────────────

func mustJSON(v any) string {
	if v == nil {
		return "null"
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "null"
	}
	return string(b)
}

// ──────────────────────────────────────────────────
// Suspension model
// ──────────────────────────────────────────────────

// suspensionModel stores Pending and Continuation as native BSON rather
// than as encoded JSON strings, the same way checkpointModel stores
// Decision: mongo is a document store, so a nested document reads and
// indexes as itself instead of as an opaque blob. The postgres/sqlite
// models encode these two to JSON text because their columns are
// JSONB/TEXT; all three round-trip the same Go values.
type suspensionModel struct {
	grove.BaseModel `grove:"table:cortex_suspensions"`
	ID              string                   `grove:"id,pk"        bson:"_id"`
	RunID           string                   `grove:"run_id"       bson:"run_id"`
	Reason          string                   `grove:"reason"       bson:"reason"`
	Pending         []suspension.PendingCall `grove:"pending"      bson:"pending"`
	Continuation    suspension.Continuation  `grove:"continuation" bson:"continuation"`
	ExpiresAt       *time.Time               `grove:"expires_at"   bson:"expires_at,omitempty"`
	ScopeL0         string                   `grove:"scope_l0"     bson:"scope_l0"`
	ScopeL1         string                   `grove:"scope_l1"     bson:"scope_l1"`
	ScopeL2         string                   `grove:"scope_l2"     bson:"scope_l2"`
	ScopeExtra      map[string]string        `grove:"scope_extra"  bson:"scope_extra,omitempty"`
	ScopeCanon      string                   `grove:"scope_canon"  bson:"scope_canon"`
	CreatedAt       time.Time                `grove:"created_at"   bson:"created_at"`
	UpdatedAt       time.Time                `grove:"updated_at"   bson:"updated_at"`
}

func suspensionToModel(s *suspension.Suspension) *suspensionModel {
	l0, l1, l2, extra := scopeColumns(s.Scope)
	// Pending's zero value is a nil slice, which bson writes as null
	// rather than as an empty array. Coerce it so a reader always gets an
	// array back, matching what postgres/sqlite store.
	pending := s.Pending
	if pending == nil {
		pending = []suspension.PendingCall{}
	}
	return &suspensionModel{
		ID:           s.ID.String(),
		RunID:        s.RunID.String(),
		Reason:       string(s.Reason),
		Pending:      pending,
		Continuation: s.Cont,
		ExpiresAt:    s.ExpiresAt,
		ScopeL0:      l0,
		ScopeL1:      l1,
		ScopeL2:      l2,
		ScopeExtra:   extra,
		ScopeCanon:   s.Scope.Canonical(),
		CreatedAt:    s.CreatedAt,
		UpdatedAt:    s.UpdatedAt,
	}
}

func suspensionFromModel(m *suspensionModel) (*suspension.Suspension, error) {
	suspensionID, err := id.ParseSuspensionID(m.ID)
	if err != nil {
		return nil, err
	}
	runID, err := id.ParseAgentRunID(m.RunID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("suspension %s: %w", suspensionID, err)
	}
	return &suspension.Suspension{
		Entity:    cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:        suspensionID,
		RunID:     runID,
		Scope:     scope,
		Reason:    suspension.SuspendReason(m.Reason),
		Pending:   m.Pending,
		Cont:      m.Continuation,
		ExpiresAt: m.ExpiresAt,
	}, nil
}

// ──────────────────────────────────────────────────
// Overlay model
// ──────────────────────────────────────────────────

// overlayModel stores Patches as native BSON rather than as an encoded
// JSON string, the same way suspensionModel stores Pending: mongo is a
// document store, so a nested document reads and indexes as itself
// instead of as an opaque blob. The postgres/sqlite models encode theirs
// to JSON text because their columns are JSONB/TEXT; all three
// round-trip the same Go values.
type overlayModel struct {
	grove.BaseModel `grove:"table:cortex_overlays"`
	ID              string            `grove:"id,pk"         bson:"_id"`
	AgentID         string            `grove:"agent_id"      bson:"agent_id"`
	Patches         []prompt.Patch    `grove:"patches"       bson:"patches"`
	ToolsAdded      []string          `grove:"tools_added"   bson:"tools_added"`
	ToolsRemoved    []string          `grove:"tools_removed" bson:"tools_removed"`
	Model           string            `grove:"model"         bson:"model"`
	Temperature     *float64          `grove:"temperature"   bson:"temperature,omitempty"`
	MaxTokens       *int              `grove:"max_tokens"    bson:"max_tokens,omitempty"`
	ScopeL0         string            `grove:"scope_l0"      bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"      bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"      bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"   bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"   bson:"scope_canon"`
	CreatedAt       time.Time         `grove:"created_at"    bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"    bson:"updated_at"`
}

func overlayToModel(o *prompt.Overlay) *overlayModel {
	l0, l1, l2, extra := scopeColumns(o.Scope)
	// A nil slice's zero value writes as bson null rather than as an
	// empty array. Coerce all three so a reader always gets an array
	// back, matching what postgres/sqlite store.
	patches := o.Patches
	if patches == nil {
		patches = []prompt.Patch{}
	}
	added := o.ToolsAdded
	if added == nil {
		added = []string{}
	}
	removed := o.ToolsRemoved
	if removed == nil {
		removed = []string{}
	}
	return &overlayModel{
		ID:           o.ID.String(),
		AgentID:      o.AgentID.String(),
		Patches:      patches,
		ToolsAdded:   added,
		ToolsRemoved: removed,
		Model:        o.Model,
		Temperature:  o.Temperature,
		MaxTokens:    o.MaxTokens,
		ScopeL0:      l0,
		ScopeL1:      l1,
		ScopeL2:      l2,
		ScopeExtra:   extra,
		ScopeCanon:   o.Scope.Canonical(),
		CreatedAt:    o.CreatedAt,
		UpdatedAt:    o.UpdatedAt,
	}
}

func overlayFromModel(m *overlayModel) (*prompt.Overlay, error) {
	overlayID, err := id.ParseOverlayID(m.ID)
	if err != nil {
		return nil, err
	}
	agentID, err := id.ParseAgentID(m.AgentID)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("overlay %s: %w", overlayID, err)
	}
	return &prompt.Overlay{
		Entity:       cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:           overlayID,
		AgentID:      agentID,
		Scope:        scope,
		Patches:      m.Patches,
		ToolsAdded:   m.ToolsAdded,
		ToolsRemoved: m.ToolsRemoved,
		Model:        m.Model,
		Temperature:  m.Temperature,
		MaxTokens:    m.MaxTokens,
	}, nil
}

// ──────────────────────────────────────────────────
// a2a models
// ──────────────────────────────────────────────────
//
// Unlike the sqlite and postgres models, these store their slice and map
// fields natively rather than as JSON strings: bson already carries the
// structure, and flattening it would make the documents unqueryable from
// a mongo shell for no gain.

type a2aMessageModel struct {
	grove.BaseModel `grove:"table:cortex_a2a_messages"`
	ID              string            `grove:"id,pk"           bson:"_id"`
	Performative    string            `grove:"performative"    bson:"performative"`
	SenderAgent     string            `grove:"sender_agent"    bson:"sender_agent"`
	SenderNode      string            `grove:"sender_node"     bson:"sender_node"`
	Receivers       []a2a.Address     `grove:"receivers"       bson:"receivers,omitempty"`
	ReplyTo         []a2a.Address     `grove:"reply_to"        bson:"reply_to,omitempty"`
	Content         string            `grove:"content"         bson:"content"`
	Language        string            `grove:"language"        bson:"language"`
	Encoding        string            `grove:"encoding"        bson:"encoding"`
	Ontology        string            `grove:"ontology"        bson:"ontology"`
	Protocol        string            `grove:"protocol"        bson:"protocol"`
	ConversationID  string            `grove:"conversation_id" bson:"conversation_id"`
	ReplyWith       string            `grove:"reply_with"      bson:"reply_with"`
	InReplyTo       string            `grove:"in_reply_to"     bson:"in_reply_to"`
	ReplyBy         *time.Time        `grove:"reply_by"        bson:"reply_by,omitempty"`
	Hops            int               `grove:"hops"            bson:"hops"`
	OriginRunID     string            `grove:"origin_run_id"   bson:"origin_run_id"`
	Metadata        map[string]any    `grove:"metadata"        bson:"metadata,omitempty"`
	ScopeL0         string            `grove:"scope_l0"        bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"        bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"        bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"     bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"     bson:"scope_canon"`
	CreatedAt       time.Time         `grove:"created_at"      bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"      bson:"updated_at"`
}

func a2aMessageToModel(e *a2a.Envelope) *a2aMessageModel {
	l0, l1, l2, extra := scopeColumns(e.Scope)
	return &a2aMessageModel{
		ID:             e.ID.String(),
		Performative:   string(e.Performative),
		SenderAgent:    e.Sender.Agent,
		SenderNode:     e.Sender.Node,
		Receivers:      e.Receivers,
		ReplyTo:        e.ReplyTo,
		Content:        e.Content,
		Language:       e.Language,
		Encoding:       e.Encoding,
		Ontology:       e.Ontology,
		Protocol:       e.Protocol,
		ConversationID: e.ConversationID.String(),
		ReplyWith:      e.ReplyWith,
		InReplyTo:      e.InReplyTo,
		ReplyBy:        e.ReplyBy,
		Hops:           e.Hops,
		OriginRunID:    e.OriginRunID.String(),
		Metadata:       e.Metadata,
		ScopeL0:        l0,
		ScopeL1:        l1,
		ScopeL2:        l2,
		ScopeExtra:     extra,
		ScopeCanon:     e.Scope.Canonical(),
		CreatedAt:      e.CreatedAt,
		UpdatedAt:      e.UpdatedAt,
	}
}

func a2aMessageFromModel(m *a2aMessageModel) (*a2a.Envelope, error) {
	msgID, err := id.ParseWithPrefix(m.ID, id.PrefixMessage)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("a2a message %s: %w", msgID, err)
	}
	e := &a2a.Envelope{
		Entity:       cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:           msgID,
		Scope:        scope,
		Performative: a2a.Performative(m.Performative),
		Sender:       a2a.Address{Agent: m.SenderAgent, Node: m.SenderNode},
		Receivers:    m.Receivers,
		ReplyTo:      m.ReplyTo,
		Content:      m.Content,
		Language:     m.Language,
		Encoding:     m.Encoding,
		Ontology:     m.Ontology,
		Protocol:     m.Protocol,
		ReplyWith:    m.ReplyWith,
		InReplyTo:    m.InReplyTo,
		ReplyBy:      m.ReplyBy,
		Hops:         m.Hops,
		Metadata:     m.Metadata,
	}
	if m.ConversationID != "" {
		convID, convErr := id.ParseWithPrefix(m.ConversationID, id.PrefixConversation)
		if convErr != nil {
			return nil, fmt.Errorf("a2a message %s: conversation id: %w", msgID, convErr)
		}
		e.ConversationID = convID
	}
	if m.OriginRunID != "" {
		runID, runErr := id.ParseWithPrefix(m.OriginRunID, id.PrefixAgentRun)
		if runErr != nil {
			return nil, fmt.Errorf("a2a message %s: origin run id: %w", msgID, runErr)
		}
		e.OriginRunID = runID
	}
	return e, nil
}

type a2aConversationModel struct {
	grove.BaseModel `grove:"table:cortex_a2a_conversations"`
	ID              string            `grove:"id,pk"            bson:"_id"`
	Protocol        string            `grove:"protocol"         bson:"protocol"`
	InitiatorAgent  string            `grove:"initiator_agent"  bson:"initiator_agent"`
	InitiatorNode   string            `grove:"initiator_node"   bson:"initiator_node"`
	Participants    []a2a.Address     `grove:"participants"     bson:"participants,omitempty"`
	Status          string            `grove:"status"           bson:"status"`
	HopCeiling      int               `grove:"hop_ceiling"      bson:"hop_ceiling"`
	HopsUsed        int               `grove:"hops_used"        bson:"hops_used"`
	Deadline        *time.Time        `grove:"deadline"         bson:"deadline,omitempty"`
	ScopeL0         string            `grove:"scope_l0"         bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"         bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"         bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"      bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"      bson:"scope_canon"`
	CreatedAt       time.Time         `grove:"created_at"       bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"       bson:"updated_at"`
}

func a2aConversationToModel(c *a2a.Conversation) *a2aConversationModel {
	l0, l1, l2, extra := scopeColumns(c.Scope)
	return &a2aConversationModel{
		ID:             c.ID.String(),
		Protocol:       c.Protocol,
		InitiatorAgent: c.Initiator.Agent,
		InitiatorNode:  c.Initiator.Node,
		Participants:   c.Participants,
		Status:         c.Status,
		HopCeiling:     c.HopCeiling,
		HopsUsed:       c.HopsUsed,
		Deadline:       c.Deadline,
		ScopeL0:        l0,
		ScopeL1:        l1,
		ScopeL2:        l2,
		ScopeExtra:     extra,
		ScopeCanon:     c.Scope.Canonical(),
		CreatedAt:      c.CreatedAt,
		UpdatedAt:      c.UpdatedAt,
	}
}

func a2aConversationFromModel(m *a2aConversationModel) (*a2a.Conversation, error) {
	convID, err := id.ParseWithPrefix(m.ID, id.PrefixConversation)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("a2a conversation %s: %w", convID, err)
	}
	return &a2a.Conversation{
		Entity:       cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:           convID,
		Scope:        scope,
		Protocol:     m.Protocol,
		Initiator:    a2a.Address{Agent: m.InitiatorAgent, Node: m.InitiatorNode},
		Participants: m.Participants,
		Status:       m.Status,
		HopCeiling:   m.HopCeiling,
		HopsUsed:     m.HopsUsed,
		Deadline:     m.Deadline,
	}, nil
}

type a2aDeliveryModel struct {
	grove.BaseModel `grove:"table:cortex_a2a_deliveries"`
	ID              string            `grove:"id,pk"          bson:"_id"`
	MessageID       string            `grove:"message_id"     bson:"message_id"`
	ReceiverAgent   string            `grove:"receiver_agent" bson:"receiver_agent"`
	ReceiverNode    string            `grove:"receiver_node"  bson:"receiver_node"`
	State           string            `grove:"state"          bson:"state"`
	Error           string            `grove:"error"          bson:"error"`
	ClaimedAt       *time.Time        `grove:"claimed_at"     bson:"claimed_at,omitempty"`
	DeliveredAt     *time.Time        `grove:"delivered_at"   bson:"delivered_at,omitempty"`
	ReadAt          *time.Time        `grove:"read_at"        bson:"read_at,omitempty"`
	RunID           string            `grove:"run_id"         bson:"run_id"`
	ScopeL0         string            `grove:"scope_l0"       bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"       bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"       bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"    bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"    bson:"scope_canon"`
	CreatedAt       time.Time         `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"     bson:"updated_at"`
}

func a2aDeliveryToModel(d *a2a.Delivery) *a2aDeliveryModel {
	l0, l1, l2, extra := scopeColumns(d.Scope)
	return &a2aDeliveryModel{
		ID:            d.ID.String(),
		MessageID:     d.MessageID.String(),
		ReceiverAgent: d.Receiver.Agent,
		ReceiverNode:  d.Receiver.Node,
		State:         d.State,
		Error:         d.Error,
		ClaimedAt:     d.ClaimedAt,
		DeliveredAt:   d.DeliveredAt,
		ReadAt:        d.ReadAt,
		RunID:         d.RunID.String(),
		ScopeL0:       l0,
		ScopeL1:       l1,
		ScopeL2:       l2,
		ScopeExtra:    extra,
		ScopeCanon:    d.Scope.Canonical(),
		CreatedAt:     d.CreatedAt,
		UpdatedAt:     d.UpdatedAt,
	}
}

func a2aDeliveryFromModel(m *a2aDeliveryModel) (*a2a.Delivery, error) {
	dlvID, err := id.ParseWithPrefix(m.ID, id.PrefixDelivery)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("a2a delivery %s: %w", dlvID, err)
	}
	msgID, err := id.ParseWithPrefix(m.MessageID, id.PrefixMessage)
	if err != nil {
		return nil, fmt.Errorf("a2a delivery %s: message id: %w", dlvID, err)
	}
	d := &a2a.Delivery{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          dlvID,
		Scope:       scope,
		MessageID:   msgID,
		Receiver:    a2a.Address{Agent: m.ReceiverAgent, Node: m.ReceiverNode},
		State:       m.State,
		Error:       m.Error,
		ClaimedAt:   m.ClaimedAt,
		DeliveredAt: m.DeliveredAt,
		ReadAt:      m.ReadAt,
	}
	if m.RunID != "" {
		runID, runErr := id.ParseWithPrefix(m.RunID, id.PrefixAgentRun)
		if runErr != nil {
			return nil, fmt.Errorf("a2a delivery %s: run id: %w", dlvID, runErr)
		}
		d.RunID = runID
	}
	return d, nil
}

type a2aPendingAskModel struct {
	grove.BaseModel `grove:"table:cortex_a2a_pending_asks"`
	ReplyWith       string            `grove:"reply_with,pk"   bson:"_id"`
	ConversationID  string            `grove:"conversation_id" bson:"conversation_id"`
	MessageID       string            `grove:"message_id"      bson:"message_id"`
	AskerRunID      string            `grove:"asker_run_id"    bson:"asker_run_id"`
	AskerAgent      string            `grove:"asker_agent"     bson:"asker_agent"`
	ToolCallID      string            `grove:"tool_call_id"    bson:"tool_call_id"`
	ExpectedAgent   string            `grove:"expected_agent"  bson:"expected_agent"`
	ExpectedNode    string            `grove:"expected_node"   bson:"expected_node"`
	Deadline        *time.Time        `grove:"deadline"        bson:"deadline,omitempty"`
	ClaimedAt       *time.Time        `grove:"claimed_at"      bson:"claimed_at,omitempty"`
	ScopeL0         string            `grove:"scope_l0"        bson:"scope_l0"`
	ScopeL1         string            `grove:"scope_l1"        bson:"scope_l1"`
	ScopeL2         string            `grove:"scope_l2"        bson:"scope_l2"`
	ScopeExtra      map[string]string `grove:"scope_extra"     bson:"scope_extra,omitempty"`
	ScopeCanon      string            `grove:"scope_canon"     bson:"scope_canon"`
	CreatedAt       time.Time         `grove:"created_at"      bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"      bson:"updated_at"`
}

func a2aPendingAskToModel(a *a2a.PendingAsk) *a2aPendingAskModel {
	l0, l1, l2, extra := scopeColumns(a.Scope)
	return &a2aPendingAskModel{
		ReplyWith:      a.ReplyWith,
		ConversationID: a.ConversationID.String(),
		MessageID:      a.MessageID.String(),
		AskerRunID:     a.AskerRunID.String(),
		AskerAgent:     a.AskerAgent,
		ToolCallID:     a.ToolCallID,
		ExpectedAgent:  a.Expected.Agent,
		ExpectedNode:   a.Expected.Node,
		Deadline:       a.Deadline,
		ClaimedAt:      a.ClaimedAt,
		ScopeL0:        l0,
		ScopeL1:        l1,
		ScopeL2:        l2,
		ScopeExtra:     extra,
		ScopeCanon:     a.Scope.Canonical(),
		CreatedAt:      a.CreatedAt,
		UpdatedAt:      a.UpdatedAt,
	}
}

func a2aPendingAskFromModel(m *a2aPendingAskModel) (*a2a.PendingAsk, error) {
	scope, err := cortex.ParseCanonical(m.ScopeCanon)
	if err != nil {
		return nil, fmt.Errorf("a2a pending ask %s: %w", m.ReplyWith, err)
	}
	a := &a2a.PendingAsk{
		Entity:     cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		Scope:      scope,
		ReplyWith:  m.ReplyWith,
		AskerAgent: m.AskerAgent,
		ToolCallID: m.ToolCallID,
		Expected:   a2a.Address{Agent: m.ExpectedAgent, Node: m.ExpectedNode},
		Deadline:   m.Deadline,
		ClaimedAt:  m.ClaimedAt,
	}
	if m.ConversationID != "" {
		convID, convErr := id.ParseWithPrefix(m.ConversationID, id.PrefixConversation)
		if convErr != nil {
			return nil, fmt.Errorf("a2a pending ask %s: conversation id: %w", m.ReplyWith, convErr)
		}
		a.ConversationID = convID
	}
	if m.MessageID != "" {
		msgID, msgErr := id.ParseWithPrefix(m.MessageID, id.PrefixMessage)
		if msgErr != nil {
			return nil, fmt.Errorf("a2a pending ask %s: message id: %w", m.ReplyWith, msgErr)
		}
		a.MessageID = msgID
	}
	if m.AskerRunID != "" {
		runID, runErr := id.ParseWithPrefix(m.AskerRunID, id.PrefixAgentRun)
		if runErr != nil {
			return nil, fmt.Errorf("a2a pending ask %s: asker run id: %w", m.ReplyWith, runErr)
		}
		a.AskerRunID = runID
	}
	return a, nil
}
