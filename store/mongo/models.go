package mongo

import (
	"encoding/json"
	"time"

	"github.com/xraph/grove"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/communication"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/perception"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/trait"
)

// ──────────────────────────────────────────────────
// Agent model
// ──────────────────────────────────────────────────

type agentModel struct {
	grove.BaseModel `grove:"table:cortex_agents"`
	ID              string         `grove:"id,pk"              bson:"_id"`
	Name            string         `grove:"name"               bson:"name"`
	Description     string         `grove:"description"        bson:"description"`
	AppID           string         `grove:"app_id"             bson:"app_id"`
	SystemPrompt    string         `grove:"system_prompt"      bson:"system_prompt"`
	Model           string         `grove:"model"              bson:"model"`
	Tools           []string       `grove:"tools"              bson:"tools,omitempty"`
	MaxSteps        int            `grove:"max_steps"          bson:"max_steps"`
	MaxTokens       int            `grove:"max_tokens"         bson:"max_tokens"`
	Temperature     float64        `grove:"temperature"        bson:"temperature"`
	ReasoningLoop   string         `grove:"reasoning_loop"     bson:"reasoning_loop"`
	Guardrails      map[string]any `grove:"guardrails"         bson:"guardrails,omitempty"`
	Metadata        map[string]any `grove:"metadata"           bson:"metadata,omitempty"`
	Enabled         bool           `grove:"enabled"            bson:"enabled"`
	PersonaRef      string         `grove:"persona_ref"        bson:"persona_ref"`
	InlineSkills    []string       `grove:"inline_skills"      bson:"inline_skills,omitempty"`
	InlineTraits    []string       `grove:"inline_traits"      bson:"inline_traits,omitempty"`
	InlineBehaviors []string       `grove:"inline_behaviors"   bson:"inline_behaviors,omitempty"`
	CreatedAt       time.Time      `grove:"created_at"         bson:"created_at"`
	UpdatedAt       time.Time      `grove:"updated_at"         bson:"updated_at"`
}

func agentToModel(c *agent.Config) *agentModel {
	return &agentModel{
		ID:              c.ID.String(),
		Name:            c.Name,
		Description:     c.Description,
		AppID:           c.AppID,
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
		CreatedAt:       c.CreatedAt,
		UpdatedAt:       c.UpdatedAt,
	}
}

func agentFromModel(m *agentModel) (*agent.Config, error) {
	agentID, err := id.ParseAgentID(m.ID)
	if err != nil {
		return nil, err
	}
	return &agent.Config{
		Entity:          cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:              agentID,
		Name:            m.Name,
		Description:     m.Description,
		AppID:           m.AppID,
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
	}, nil
}

// ──────────────────────────────────────────────────
// Run model
// ──────────────────────────────────────────────────

type runModel struct {
	grove.BaseModel `grove:"table:cortex_runs"`
	ID              string         `grove:"id,pk"          bson:"_id"`
	AgentID         string         `grove:"agent_id"       bson:"agent_id"`
	TenantID        string         `grove:"tenant_id"      bson:"tenant_id"`
	State           string         `grove:"state"          bson:"state"`
	Input           string         `grove:"input"          bson:"input"`
	Output          string         `grove:"output"         bson:"output"`
	Error           string         `grove:"error"          bson:"error"`
	StepCount       int            `grove:"step_count"     bson:"step_count"`
	TokensUsed      int            `grove:"tokens_used"    bson:"tokens_used"`
	StartedAt       *time.Time     `grove:"started_at"     bson:"started_at,omitempty"`
	CompletedAt     *time.Time     `grove:"completed_at"   bson:"completed_at,omitempty"`
	PersonaRef      string         `grove:"persona_ref"    bson:"persona_ref"`
	Metadata        map[string]any `grove:"metadata"       bson:"metadata,omitempty"`
	CreatedAt       time.Time      `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time      `grove:"updated_at"     bson:"updated_at"`
}

func runToModel(r *run.Run) *runModel {
	return &runModel{
		ID:          r.ID.String(),
		AgentID:     r.AgentID.String(),
		TenantID:    r.TenantID,
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
	return &run.Run{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          runID,
		AgentID:     agentID,
		TenantID:    m.TenantID,
		State:       run.RunState(m.State),
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
	ID              string         `grove:"id,pk"          bson:"_id"`
	RunID           string         `grove:"run_id"         bson:"run_id"`
	Index           int            `grove:"index"          bson:"index"`
	Type            string         `grove:"type"           bson:"type"`
	Input           string         `grove:"input"          bson:"input"`
	Output          string         `grove:"output"         bson:"output"`
	TokensUsed      int            `grove:"tokens_used"    bson:"tokens_used"`
	StartedAt       *time.Time     `grove:"started_at"     bson:"started_at,omitempty"`
	CompletedAt     *time.Time     `grove:"completed_at"   bson:"completed_at,omitempty"`
	Metadata        map[string]any `grove:"metadata"       bson:"metadata,omitempty"`
	CreatedAt       time.Time      `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time      `grove:"updated_at"     bson:"updated_at"`
}

func stepToModel(s *run.Step) *stepModel {
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
	return &run.Step{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          stepID,
		RunID:       runID,
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
	ID              string         `grove:"id,pk"          bson:"_id"`
	StepID          string         `grove:"step_id"        bson:"step_id"`
	RunID           string         `grove:"run_id"         bson:"run_id"`
	ToolName        string         `grove:"tool_name"      bson:"tool_name"`
	Arguments       string         `grove:"arguments"      bson:"arguments"`
	Result          string         `grove:"result"         bson:"result"`
	Error           string         `grove:"error"          bson:"error"`
	StartedAt       *time.Time     `grove:"started_at"     bson:"started_at,omitempty"`
	CompletedAt     *time.Time     `grove:"completed_at"   bson:"completed_at,omitempty"`
	Metadata        map[string]any `grove:"metadata"       bson:"metadata,omitempty"`
	CreatedAt       time.Time      `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time      `grove:"updated_at"     bson:"updated_at"`
}

func toolCallToModel(tc *run.ToolCall) *toolCallModel {
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
	return &run.ToolCall{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          tcID,
		StepID:      stepID,
		RunID:       runID,
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
	ID              string         `grove:"id,pk"          bson:"_id,omitempty"`
	AgentID         string         `grove:"agent_id"       bson:"agent_id"`
	TenantID        string         `grove:"tenant_id"      bson:"tenant_id"`
	Kind            string         `grove:"kind"           bson:"kind"`
	Key             string         `grove:"key"            bson:"key"`
	Content         string         `grove:"content"        bson:"content"`
	Metadata        map[string]any `grove:"metadata"       bson:"metadata,omitempty"`
	CreatedAt       time.Time      `grove:"created_at"     bson:"created_at"`
}

func messageToModel(agentID, tenantID string, msg memory.Message) *memoryModel {
	return &memoryModel{
		AgentID:  agentID,
		TenantID: tenantID,
		Kind:     "conversation",
		Content:  mustJSON(msg),
		Metadata: msg.Metadata,
	}
}

// ──────────────────────────────────────────────────
// Checkpoint model
// ──────────────────────────────────────────────────

type checkpointModel struct {
	grove.BaseModel `grove:"table:cortex_checkpoints"`
	ID              string               `grove:"id,pk"          bson:"_id"`
	RunID           string               `grove:"run_id"         bson:"run_id"`
	AgentID         string               `grove:"agent_id"       bson:"agent_id"`
	TenantID        string               `grove:"tenant_id"      bson:"tenant_id"`
	Reason          string               `grove:"reason"         bson:"reason"`
	StepIndex       int                  `grove:"step_index"     bson:"step_index"`
	State           string               `grove:"state"          bson:"state"`
	Decision        *checkpoint.Decision `grove:"decision"       bson:"decision,omitempty"`
	Metadata        map[string]any       `grove:"metadata"       bson:"metadata,omitempty"`
	CreatedAt       time.Time            `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time            `grove:"updated_at"     bson:"updated_at"`
}

func checkpointToModel(cp *checkpoint.Checkpoint) *checkpointModel {
	return &checkpointModel{
		ID:        cp.ID.String(),
		RunID:     cp.RunID.String(),
		AgentID:   cp.AgentID.String(),
		TenantID:  cp.TenantID,
		Reason:    cp.Reason,
		StepIndex: cp.StepIndex,
		State:     cp.State,
		Decision:  cp.Decision,
		Metadata:  cp.Metadata,
		CreatedAt: cp.CreatedAt,
		UpdatedAt: cp.UpdatedAt,
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
	return &checkpoint.Checkpoint{
		Entity:    cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:        cpID,
		RunID:     runID,
		AgentID:   agentID,
		TenantID:  m.TenantID,
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
	CreatedAt            time.Time            `grove:"created_at"             bson:"created_at"`
	UpdatedAt            time.Time            `grove:"updated_at"             bson:"updated_at"`
}

func skillToModel(s *skill.Skill) *skillModel {
	return &skillModel{
		ID:                   s.ID.String(),
		Name:                 s.Name,
		Description:          s.Description,
		AppID:                s.AppID,
		Tools:                s.Tools,
		Knowledge:            s.Knowledge,
		SystemPromptFragment: s.SystemPromptFragment,
		Dependencies:         s.Dependencies,
		DefaultProficiency:   string(s.DefaultProficiency),
		Metadata:             s.Metadata,
		CreatedAt:            s.CreatedAt,
		UpdatedAt:            s.UpdatedAt,
	}
}

func skillFromModel(m *skillModel) (*skill.Skill, error) {
	skillID, err := id.ParseSkillID(m.ID)
	if err != nil {
		return nil, err
	}
	return &skill.Skill{
		Entity:               cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:                   skillID,
		Name:                 m.Name,
		Description:          m.Description,
		AppID:                m.AppID,
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
	CreatedAt       time.Time         `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time         `grove:"updated_at"     bson:"updated_at"`
}

func traitToModel(t *trait.Trait) *traitModel {
	return &traitModel{
		ID:          t.ID.String(),
		Name:        t.Name,
		Description: t.Description,
		AppID:       t.AppID,
		Dimensions:  t.Dimensions,
		Influences:  t.Influences,
		Category:    string(t.Category),
		Metadata:    t.Metadata,
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	}
}

func traitFromModel(m *traitModel) (*trait.Trait, error) {
	traitID, err := id.ParseTraitID(m.ID)
	if err != nil {
		return nil, err
	}
	return &trait.Trait{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          traitID,
		Name:        m.Name,
		Description: m.Description,
		AppID:       m.AppID,
		Dimensions:  m.Dimensions,
		Influences:  m.Influences,
		Category:    trait.TraitCategory(m.Category),
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
	CreatedAt       time.Time          `grove:"created_at"     bson:"created_at"`
	UpdatedAt       time.Time          `grove:"updated_at"     bson:"updated_at"`
}

func behaviorToModel(b *behavior.Behavior) *behaviorModel {
	return &behaviorModel{
		ID:            b.ID.String(),
		Name:          b.Name,
		Description:   b.Description,
		AppID:         b.AppID,
		Triggers:      b.Triggers,
		Actions:       b.Actions,
		Priority:      b.Priority,
		RequiresSkill: b.RequiresSkill,
		RequiresTrait: b.RequiresTrait,
		Metadata:      b.Metadata,
		CreatedAt:     b.CreatedAt,
		UpdatedAt:     b.UpdatedAt,
	}
}

func behaviorFromModel(m *behaviorModel) (*behavior.Behavior, error) {
	behaviorID, err := id.ParseBehaviorID(m.ID)
	if err != nil {
		return nil, err
	}
	return &behavior.Behavior{
		Entity:        cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:            behaviorID,
		Name:          m.Name,
		Description:   m.Description,
		AppID:         m.AppID,
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
	CreatedAt          time.Time                 `grove:"created_at"           bson:"created_at"`
	UpdatedAt          time.Time                 `grove:"updated_at"           bson:"updated_at"`
}

func personaToModel(p *persona.Persona) *personaModel {
	return &personaModel{
		ID:                 p.ID.String(),
		Name:               p.Name,
		Description:        p.Description,
		AppID:              p.AppID,
		Identity:           p.Identity,
		Skills:             p.Skills,
		Traits:             p.Traits,
		Behaviors:          p.Behaviors,
		CognitiveStyle:     p.CognitiveStyle,
		CommunicationStyle: p.CommunicationStyle,
		Perception:         p.Perception,
		Metadata:           p.Metadata,
		CreatedAt:          p.CreatedAt,
		UpdatedAt:          p.UpdatedAt,
	}
}

func personaFromModel(m *personaModel) (*persona.Persona, error) {
	personaID, err := id.ParsePersonaID(m.ID)
	if err != nil {
		return nil, err
	}
	return &persona.Persona{
		Entity:             cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:                 personaID,
		Name:               m.Name,
		Description:        m.Description,
		AppID:              m.AppID,
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
