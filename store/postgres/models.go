package postgres

import (
	"encoding/json"
	"time"

	"github.com/uptrace/bun"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/trait"
)

// ──────────────────────────────────────────────────
// Agent model
// ──────────────────────────────────────────────────

type agentModel struct {
	bun.BaseModel `bun:"table:cortex_agents"`
	ID            string    `bun:"id,pk"`
	Name          string    `bun:"name,notnull"`
	Description   string    `bun:"description"`
	AppID         string    `bun:"app_id,notnull"`
	SystemPrompt  string    `bun:"system_prompt"`
	Model         string    `bun:"model"`
	Tools         string    `bun:"tools,type:jsonb"`
	MaxSteps      int       `bun:"max_steps"`
	MaxTokens     int       `bun:"max_tokens"`
	Temperature   float64   `bun:"temperature"`
	ReasoningLoop string    `bun:"reasoning_loop"`
	Guardrails    string    `bun:"guardrails,type:jsonb"`
	Metadata      string    `bun:"metadata,type:jsonb"`
	Enabled       bool      `bun:"enabled"`
	PersonaRef    string    `bun:"persona_ref"`
	InlineSkills  string    `bun:"inline_skills,type:jsonb"`
	InlineTraits  string    `bun:"inline_traits,type:jsonb"`
	InlineBehaviors string  `bun:"inline_behaviors,type:jsonb"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt     time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}

func agentToModel(c *agent.Config) *agentModel {
	return &agentModel{
		ID:              c.ID.String(),
		Name:            c.Name,
		Description:     c.Description,
		AppID:           c.AppID,
		SystemPrompt:    c.SystemPrompt,
		Model:           c.Model,
		Tools:           mustJSON(c.Tools),
		MaxSteps:        c.MaxSteps,
		MaxTokens:       c.MaxTokens,
		Temperature:     c.Temperature,
		ReasoningLoop:   c.ReasoningLoop,
		Guardrails:      mustJSON(c.Guardrails),
		Metadata:        mustJSON(c.Metadata),
		Enabled:         c.Enabled,
		PersonaRef:      c.PersonaRef,
		InlineSkills:    mustJSON(c.InlineSkills),
		InlineTraits:    mustJSON(c.InlineTraits),
		InlineBehaviors: mustJSON(c.InlineBehaviors),
		CreatedAt:       c.CreatedAt,
		UpdatedAt:       c.UpdatedAt,
	}
}

func agentFromModel(m *agentModel) *agent.Config {
	agentID, _ := id.ParseAgentID(m.ID)
	c := &agent.Config{
		Entity:        cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:            agentID,
		Name:          m.Name,
		Description:   m.Description,
		AppID:         m.AppID,
		SystemPrompt:  m.SystemPrompt,
		Model:         m.Model,
		MaxSteps:      m.MaxSteps,
		MaxTokens:     m.MaxTokens,
		Temperature:   m.Temperature,
		ReasoningLoop: m.ReasoningLoop,
		Enabled:       m.Enabled,
		PersonaRef:    m.PersonaRef,
	}
	_ = json.Unmarshal([]byte(m.Tools), &c.Tools)
	_ = json.Unmarshal([]byte(m.Guardrails), &c.Guardrails)
	_ = json.Unmarshal([]byte(m.Metadata), &c.Metadata)
	_ = json.Unmarshal([]byte(m.InlineSkills), &c.InlineSkills)
	_ = json.Unmarshal([]byte(m.InlineTraits), &c.InlineTraits)
	_ = json.Unmarshal([]byte(m.InlineBehaviors), &c.InlineBehaviors)
	return c
}

// ──────────────────────────────────────────────────
// Skill model
// ──────────────────────────────────────────────────

type skillModel struct {
	bun.BaseModel        `bun:"table:cortex_skills"`
	ID                   string    `bun:"id,pk"`
	Name                 string    `bun:"name,notnull"`
	Description          string    `bun:"description"`
	AppID                string    `bun:"app_id,notnull"`
	Tools                string    `bun:"tools,type:jsonb"`
	Knowledge            string    `bun:"knowledge,type:jsonb"`
	SystemPromptFragment string    `bun:"system_prompt_fragment"`
	Dependencies         string    `bun:"dependencies,type:jsonb"`
	DefaultProficiency   string    `bun:"default_proficiency"`
	Metadata             string    `bun:"metadata,type:jsonb"`
	CreatedAt            time.Time `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt            time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}

func skillToModel(s *skill.Skill) *skillModel {
	return &skillModel{
		ID:                   s.ID.String(),
		Name:                 s.Name,
		Description:          s.Description,
		AppID:                s.AppID,
		Tools:                mustJSON(s.Tools),
		Knowledge:            mustJSON(s.Knowledge),
		SystemPromptFragment: s.SystemPromptFragment,
		Dependencies:         mustJSON(s.Dependencies),
		DefaultProficiency:   string(s.DefaultProficiency),
		Metadata:             mustJSON(s.Metadata),
		CreatedAt:            s.CreatedAt,
		UpdatedAt:            s.UpdatedAt,
	}
}

func skillFromModel(m *skillModel) *skill.Skill {
	skillID, _ := id.ParseSkillID(m.ID)
	s := &skill.Skill{
		Entity:               cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:                   skillID,
		Name:                 m.Name,
		Description:          m.Description,
		AppID:                m.AppID,
		SystemPromptFragment: m.SystemPromptFragment,
		DefaultProficiency:   skill.Proficiency(m.DefaultProficiency),
	}
	_ = json.Unmarshal([]byte(m.Tools), &s.Tools)
	_ = json.Unmarshal([]byte(m.Knowledge), &s.Knowledge)
	_ = json.Unmarshal([]byte(m.Dependencies), &s.Dependencies)
	_ = json.Unmarshal([]byte(m.Metadata), &s.Metadata)
	return s
}

// ──────────────────────────────────────────────────
// Trait model
// ──────────────────────────────────────────────────

type traitModel struct {
	bun.BaseModel `bun:"table:cortex_traits"`
	ID            string    `bun:"id,pk"`
	Name          string    `bun:"name,notnull"`
	Description   string    `bun:"description"`
	AppID         string    `bun:"app_id,notnull"`
	Dimensions    string    `bun:"dimensions,type:jsonb"`
	Influences    string    `bun:"influences,type:jsonb"`
	Category      string    `bun:"category"`
	Metadata      string    `bun:"metadata,type:jsonb"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt     time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}

func traitToModel(t *trait.Trait) *traitModel {
	return &traitModel{
		ID:          t.ID.String(),
		Name:        t.Name,
		Description: t.Description,
		AppID:       t.AppID,
		Dimensions:  mustJSON(t.Dimensions),
		Influences:  mustJSON(t.Influences),
		Category:    string(t.Category),
		Metadata:    mustJSON(t.Metadata),
		CreatedAt:   t.CreatedAt,
		UpdatedAt:   t.UpdatedAt,
	}
}

func traitFromModel(m *traitModel) *trait.Trait {
	traitID, _ := id.ParseTraitID(m.ID)
	t := &trait.Trait{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          traitID,
		Name:        m.Name,
		Description: m.Description,
		AppID:       m.AppID,
		Category:    trait.TraitCategory(m.Category),
	}
	_ = json.Unmarshal([]byte(m.Dimensions), &t.Dimensions)
	_ = json.Unmarshal([]byte(m.Influences), &t.Influences)
	_ = json.Unmarshal([]byte(m.Metadata), &t.Metadata)
	return t
}

// ──────────────────────────────────────────────────
// Behavior model
// ──────────────────────────────────────────────────

type behaviorModel struct {
	bun.BaseModel `bun:"table:cortex_behaviors"`
	ID            string    `bun:"id,pk"`
	Name          string    `bun:"name,notnull"`
	Description   string    `bun:"description"`
	AppID         string    `bun:"app_id,notnull"`
	Triggers      string    `bun:"triggers,type:jsonb"`
	Actions       string    `bun:"actions,type:jsonb"`
	Priority      int       `bun:"priority"`
	RequiresSkill string    `bun:"requires_skill"`
	RequiresTrait string    `bun:"requires_trait"`
	Metadata      string    `bun:"metadata,type:jsonb"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt     time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}

func behaviorToModel(b *behavior.Behavior) *behaviorModel {
	return &behaviorModel{
		ID:            b.ID.String(),
		Name:          b.Name,
		Description:   b.Description,
		AppID:         b.AppID,
		Triggers:      mustJSON(b.Triggers),
		Actions:       mustJSON(b.Actions),
		Priority:      b.Priority,
		RequiresSkill: b.RequiresSkill,
		RequiresTrait: b.RequiresTrait,
		Metadata:      mustJSON(b.Metadata),
		CreatedAt:     b.CreatedAt,
		UpdatedAt:     b.UpdatedAt,
	}
}

func behaviorFromModel(m *behaviorModel) *behavior.Behavior {
	behaviorID, _ := id.ParseBehaviorID(m.ID)
	b := &behavior.Behavior{
		Entity:        cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:            behaviorID,
		Name:          m.Name,
		Description:   m.Description,
		AppID:         m.AppID,
		Priority:      m.Priority,
		RequiresSkill: m.RequiresSkill,
		RequiresTrait: m.RequiresTrait,
	}
	_ = json.Unmarshal([]byte(m.Triggers), &b.Triggers)
	_ = json.Unmarshal([]byte(m.Actions), &b.Actions)
	_ = json.Unmarshal([]byte(m.Metadata), &b.Metadata)
	return b
}

// ──────────────────────────────────────────────────
// Persona model
// ──────────────────────────────────────────────────

type personaModel struct {
	bun.BaseModel      `bun:"table:cortex_personas"`
	ID                 string    `bun:"id,pk"`
	Name               string    `bun:"name,notnull"`
	Description        string    `bun:"description"`
	AppID              string    `bun:"app_id,notnull"`
	Identity           string    `bun:"identity"`
	Skills             string    `bun:"skills,type:jsonb"`
	Traits             string    `bun:"traits,type:jsonb"`
	Behaviors          string    `bun:"behaviors,type:jsonb"`
	CognitiveStyle     string    `bun:"cognitive_style,type:jsonb"`
	CommunicationStyle string    `bun:"communication_style,type:jsonb"`
	Perception         string    `bun:"perception,type:jsonb"`
	Metadata           string    `bun:"metadata,type:jsonb"`
	CreatedAt          time.Time `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt          time.Time `bun:"updated_at,notnull,default:current_timestamp"`
}

func personaToModel(p *persona.Persona) *personaModel {
	return &personaModel{
		ID:                 p.ID.String(),
		Name:               p.Name,
		Description:        p.Description,
		AppID:              p.AppID,
		Identity:           p.Identity,
		Skills:             mustJSON(p.Skills),
		Traits:             mustJSON(p.Traits),
		Behaviors:          mustJSON(p.Behaviors),
		CognitiveStyle:     mustJSON(p.CognitiveStyle),
		CommunicationStyle: mustJSON(p.CommunicationStyle),
		Perception:         mustJSON(p.Perception),
		Metadata:           mustJSON(p.Metadata),
		CreatedAt:          p.CreatedAt,
		UpdatedAt:          p.UpdatedAt,
	}
}

func personaFromModel(m *personaModel) *persona.Persona {
	personaID, _ := id.ParsePersonaID(m.ID)
	p := &persona.Persona{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          personaID,
		Name:        m.Name,
		Description: m.Description,
		AppID:       m.AppID,
		Identity:    m.Identity,
	}
	_ = json.Unmarshal([]byte(m.Skills), &p.Skills)
	_ = json.Unmarshal([]byte(m.Traits), &p.Traits)
	_ = json.Unmarshal([]byte(m.Behaviors), &p.Behaviors)
	_ = json.Unmarshal([]byte(m.CognitiveStyle), &p.CognitiveStyle)
	_ = json.Unmarshal([]byte(m.CommunicationStyle), &p.CommunicationStyle)
	_ = json.Unmarshal([]byte(m.Perception), &p.Perception)
	_ = json.Unmarshal([]byte(m.Metadata), &p.Metadata)
	return p
}

// ──────────────────────────────────────────────────
// Run model
// ──────────────────────────────────────────────────

type runModel struct {
	bun.BaseModel `bun:"table:cortex_runs"`
	ID            string     `bun:"id,pk"`
	AgentID       string     `bun:"agent_id,notnull"`
	TenantID      string     `bun:"tenant_id"`
	State         string     `bun:"state,notnull"`
	Input         string     `bun:"input"`
	Output        string     `bun:"output"`
	Error         string     `bun:"error"`
	StepCount     int        `bun:"step_count"`
	TokensUsed    int        `bun:"tokens_used"`
	StartedAt     *time.Time `bun:"started_at"`
	CompletedAt   *time.Time `bun:"completed_at"`
	PersonaRef    string     `bun:"persona_ref"`
	Metadata      string     `bun:"metadata,type:jsonb"`
	CreatedAt     time.Time  `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt     time.Time  `bun:"updated_at,notnull,default:current_timestamp"`
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
		Metadata:    mustJSON(r.Metadata),
		CreatedAt:   r.CreatedAt,
		UpdatedAt:   r.UpdatedAt,
	}
}

func runFromModel(m *runModel) *run.Run {
	runID, _ := id.ParseAgentRunID(m.ID)
	agentID, _ := id.ParseAgentID(m.AgentID)
	r := &run.Run{
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
	}
	_ = json.Unmarshal([]byte(m.Metadata), &r.Metadata)
	return r
}

// ──────────────────────────────────────────────────
// Step model
// ──────────────────────────────────────────────────

type stepModel struct {
	bun.BaseModel `bun:"table:cortex_steps"`
	ID            string     `bun:"id,pk"`
	RunID         string     `bun:"run_id,notnull"`
	Index         int        `bun:"index,notnull"`
	Type          string     `bun:"type"`
	Input         string     `bun:"input"`
	Output        string     `bun:"output"`
	TokensUsed    int        `bun:"tokens_used"`
	StartedAt     *time.Time `bun:"started_at"`
	CompletedAt   *time.Time `bun:"completed_at"`
	Metadata      string     `bun:"metadata,type:jsonb"`
	CreatedAt     time.Time  `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt     time.Time  `bun:"updated_at,notnull,default:current_timestamp"`
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
		Metadata:    mustJSON(s.Metadata),
		CreatedAt:   s.CreatedAt,
		UpdatedAt:   s.UpdatedAt,
	}
}

func stepFromModel(m *stepModel) *run.Step {
	stepID, _ := id.ParseStepID(m.ID)
	runID, _ := id.ParseAgentRunID(m.RunID)
	s := &run.Step{
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
	}
	_ = json.Unmarshal([]byte(m.Metadata), &s.Metadata)
	return s
}

// ──────────────────────────────────────────────────
// ToolCall model
// ──────────────────────────────────────────────────

type toolCallModel struct {
	bun.BaseModel `bun:"table:cortex_tool_calls"`
	ID            string     `bun:"id,pk"`
	StepID        string     `bun:"step_id,notnull"`
	RunID         string     `bun:"run_id,notnull"`
	ToolName      string     `bun:"tool_name,notnull"`
	Arguments     string     `bun:"arguments"`
	Result        string     `bun:"result"`
	Error         string     `bun:"error"`
	StartedAt     *time.Time `bun:"started_at"`
	CompletedAt   *time.Time `bun:"completed_at"`
	Metadata      string     `bun:"metadata,type:jsonb"`
	CreatedAt     time.Time  `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt     time.Time  `bun:"updated_at,notnull,default:current_timestamp"`
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
		Metadata:    mustJSON(tc.Metadata),
		CreatedAt:   tc.CreatedAt,
		UpdatedAt:   tc.UpdatedAt,
	}
}

func toolCallFromModel(m *toolCallModel) *run.ToolCall {
	tcID, _ := id.ParseToolCallID(m.ID)
	stepID, _ := id.ParseStepID(m.StepID)
	runID, _ := id.ParseAgentRunID(m.RunID)
	tc := &run.ToolCall{
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
	}
	_ = json.Unmarshal([]byte(m.Metadata), &tc.Metadata)
	return tc
}

// ──────────────────────────────────────────────────
// Memory model
// ──────────────────────────────────────────────────

type memoryModel struct {
	bun.BaseModel `bun:"table:cortex_memories"`
	ID            int64     `bun:"id,pk,autoincrement"`
	AgentID       string    `bun:"agent_id,notnull"`
	TenantID      string    `bun:"tenant_id"`
	Kind          string    `bun:"kind,notnull"`
	Key           string    `bun:"key"`
	Content       string    `bun:"content,notnull"`
	Metadata      string    `bun:"metadata,type:jsonb"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
}

func messageToModel(agentID, tenantID string, msg memory.Message) *memoryModel {
	return &memoryModel{
		AgentID:  agentID,
		TenantID: tenantID,
		Kind:     "conversation",
		Content:  mustJSON(msg),
		Metadata: mustJSON(msg.Metadata),
	}
}

// ──────────────────────────────────────────────────
// Checkpoint model
// ──────────────────────────────────────────────────

type checkpointModel struct {
	bun.BaseModel `bun:"table:cortex_checkpoints"`
	ID            string    `bun:"id,pk"`
	RunID         string    `bun:"run_id,notnull"`
	AgentID       string    `bun:"agent_id,notnull"`
	TenantID      string    `bun:"tenant_id"`
	Reason        string    `bun:"reason"`
	StepIndex     int       `bun:"step_index"`
	State         string    `bun:"state,notnull"`
	Decision      string    `bun:"decision,type:jsonb"`
	Metadata      string    `bun:"metadata,type:jsonb"`
	CreatedAt     time.Time `bun:"created_at,notnull,default:current_timestamp"`
	UpdatedAt     time.Time `bun:"updated_at,notnull,default:current_timestamp"`
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
		Decision:  mustJSON(cp.Decision),
		Metadata:  mustJSON(cp.Metadata),
		CreatedAt: cp.CreatedAt,
		UpdatedAt: cp.UpdatedAt,
	}
}

func checkpointFromModel(m *checkpointModel) *checkpoint.Checkpoint {
	cpID, _ := id.ParseCheckpointID(m.ID)
	runID, _ := id.ParseAgentRunID(m.RunID)
	agentID, _ := id.ParseAgentID(m.AgentID)
	cp := &checkpoint.Checkpoint{
		Entity:    cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:        cpID,
		RunID:     runID,
		AgentID:   agentID,
		TenantID:  m.TenantID,
		Reason:    m.Reason,
		StepIndex: m.StepIndex,
		State:     m.State,
	}
	_ = json.Unmarshal([]byte(m.Decision), &cp.Decision)
	_ = json.Unmarshal([]byte(m.Metadata), &cp.Metadata)
	return cp
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
