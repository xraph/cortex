package sqlite

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/xraph/grove"

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

// unmarshalField unmarshals a JSON string field into dest.
func unmarshalField(field, data string, dest any) error {
	if data == "" || data == "null" {
		return nil
	}
	if err := json.Unmarshal([]byte(data), dest); err != nil {
		return fmt.Errorf("unmarshal %s: %w", field, err)
	}
	return nil
}

// ──────────────────────────────────────────────────
// Agent model
// ──────────────────────────────────────────────────

type agentModel struct {
	grove.BaseModel `grove:"table:cortex_agents"`
	ID              string    `grove:"id,pk"`
	Name            string    `grove:"name,notnull"`
	Description     string    `grove:"description"`
	AppID           string    `grove:"app_id,notnull"`
	SystemPrompt    string    `grove:"system_prompt"`
	Model           string    `grove:"model"`
	Tools           string    `grove:"tools"`
	MaxSteps        int       `grove:"max_steps"`
	MaxTokens       int       `grove:"max_tokens"`
	Temperature     float64   `grove:"temperature"`
	ReasoningLoop   string    `grove:"reasoning_loop"`
	Guardrails      string    `grove:"guardrails"`
	Metadata        string    `grove:"metadata"`
	Enabled         bool      `grove:"enabled"`
	PersonaRef      string    `grove:"persona_ref"`
	InlineSkills    string    `grove:"inline_skills"`
	InlineTraits    string    `grove:"inline_traits"`
	InlineBehaviors string    `grove:"inline_behaviors"`
	CreatedAt       time.Time `grove:"created_at"`
	UpdatedAt       time.Time `grove:"updated_at"`
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

func agentFromModel(m *agentModel) (*agent.Config, error) {
	agentID, err := id.ParseAgentID(m.ID)
	if err != nil {
		return nil, err
	}
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
	if err := json.Unmarshal([]byte(m.Tools), &c.Tools); err != nil {
		return nil, fmt.Errorf("unmarshal tools: %w", err)
	}
	if err := json.Unmarshal([]byte(m.Guardrails), &c.Guardrails); err != nil {
		return nil, fmt.Errorf("unmarshal guardrails: %w", err)
	}
	if err := json.Unmarshal([]byte(m.Metadata), &c.Metadata); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}
	if err := json.Unmarshal([]byte(m.InlineSkills), &c.InlineSkills); err != nil {
		return nil, fmt.Errorf("unmarshal inline_skills: %w", err)
	}
	if err := json.Unmarshal([]byte(m.InlineTraits), &c.InlineTraits); err != nil {
		return nil, fmt.Errorf("unmarshal inline_traits: %w", err)
	}
	if err := json.Unmarshal([]byte(m.InlineBehaviors), &c.InlineBehaviors); err != nil {
		return nil, fmt.Errorf("unmarshal inline_behaviors: %w", err)
	}
	return c, nil
}

// ──────────────────────────────────────────────────
// Skill model
// ──────────────────────────────────────────────────

type skillModel struct {
	grove.BaseModel      `grove:"table:cortex_skills"`
	ID                   string    `grove:"id,pk"`
	Name                 string    `grove:"name,notnull"`
	Description          string    `grove:"description"`
	AppID                string    `grove:"app_id,notnull"`
	Tools                string    `grove:"tools"`
	Knowledge            string    `grove:"knowledge"`
	SystemPromptFragment string    `grove:"system_prompt_fragment"`
	Dependencies         string    `grove:"dependencies"`
	DefaultProficiency   string    `grove:"default_proficiency"`
	Metadata             string    `grove:"metadata"`
	CreatedAt            time.Time `grove:"created_at"`
	UpdatedAt            time.Time `grove:"updated_at"`
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

func skillFromModel(m *skillModel) (*skill.Skill, error) {
	skillID, err := id.ParseSkillID(m.ID)
	if err != nil {
		return nil, err
	}
	s := &skill.Skill{
		Entity:               cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:                   skillID,
		Name:                 m.Name,
		Description:          m.Description,
		AppID:                m.AppID,
		SystemPromptFragment: m.SystemPromptFragment,
		DefaultProficiency:   skill.Proficiency(m.DefaultProficiency),
	}
	for _, f := range []struct {
		name string
		data string
		dest any
	}{
		{"tools", m.Tools, &s.Tools},
		{"knowledge", m.Knowledge, &s.Knowledge},
		{"dependencies", m.Dependencies, &s.Dependencies},
		{"metadata", m.Metadata, &s.Metadata},
	} {
		if err := unmarshalField(f.name, f.data, f.dest); err != nil {
			return nil, err
		}
	}
	return s, nil
}

// ──────────────────────────────────────────────────
// Trait model
// ──────────────────────────────────────────────────

type traitModel struct {
	grove.BaseModel `grove:"table:cortex_traits"`
	ID              string    `grove:"id,pk"`
	Name            string    `grove:"name,notnull"`
	Description     string    `grove:"description"`
	AppID           string    `grove:"app_id,notnull"`
	Dimensions      string    `grove:"dimensions"`
	Influences      string    `grove:"influences"`
	Category        string    `grove:"category"`
	Metadata        string    `grove:"metadata"`
	CreatedAt       time.Time `grove:"created_at"`
	UpdatedAt       time.Time `grove:"updated_at"`
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

func traitFromModel(m *traitModel) (*trait.Trait, error) {
	traitID, err := id.ParseTraitID(m.ID)
	if err != nil {
		return nil, err
	}
	t := &trait.Trait{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          traitID,
		Name:        m.Name,
		Description: m.Description,
		AppID:       m.AppID,
		Category:    trait.Category(m.Category),
	}
	for _, f := range []struct {
		name string
		data string
		dest any
	}{
		{"dimensions", m.Dimensions, &t.Dimensions},
		{"influences", m.Influences, &t.Influences},
		{"metadata", m.Metadata, &t.Metadata},
	} {
		if err := unmarshalField(f.name, f.data, f.dest); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// ──────────────────────────────────────────────────
// Behavior model
// ──────────────────────────────────────────────────

type behaviorModel struct {
	grove.BaseModel `grove:"table:cortex_behaviors"`
	ID              string    `grove:"id,pk"`
	Name            string    `grove:"name,notnull"`
	Description     string    `grove:"description"`
	AppID           string    `grove:"app_id,notnull"`
	Triggers        string    `grove:"triggers"`
	Actions         string    `grove:"actions"`
	Priority        int       `grove:"priority"`
	RequiresSkill   string    `grove:"requires_skill"`
	RequiresTrait   string    `grove:"requires_trait"`
	Metadata        string    `grove:"metadata"`
	CreatedAt       time.Time `grove:"created_at"`
	UpdatedAt       time.Time `grove:"updated_at"`
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

func behaviorFromModel(m *behaviorModel) (*behavior.Behavior, error) {
	behaviorID, err := id.ParseBehaviorID(m.ID)
	if err != nil {
		return nil, err
	}
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
	for _, f := range []struct {
		name string
		data string
		dest any
	}{
		{"triggers", m.Triggers, &b.Triggers},
		{"actions", m.Actions, &b.Actions},
		{"metadata", m.Metadata, &b.Metadata},
	} {
		if err := unmarshalField(f.name, f.data, f.dest); err != nil {
			return nil, err
		}
	}
	return b, nil
}

// ──────────────────────────────────────────────────
// Persona model
// ──────────────────────────────────────────────────

type personaModel struct {
	grove.BaseModel    `grove:"table:cortex_personas"`
	ID                 string    `grove:"id,pk"`
	Name               string    `grove:"name,notnull"`
	Description        string    `grove:"description"`
	AppID              string    `grove:"app_id,notnull"`
	Identity           string    `grove:"identity"`
	Skills             string    `grove:"skills"`
	Traits             string    `grove:"traits"`
	Behaviors          string    `grove:"behaviors"`
	CognitiveStyle     string    `grove:"cognitive_style"`
	CommunicationStyle string    `grove:"communication_style"`
	Perception         string    `grove:"perception"`
	Metadata           string    `grove:"metadata"`
	CreatedAt          time.Time `grove:"created_at"`
	UpdatedAt          time.Time `grove:"updated_at"`
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

func personaFromModel(m *personaModel) (*persona.Persona, error) {
	personaID, err := id.ParsePersonaID(m.ID)
	if err != nil {
		return nil, err
	}
	p := &persona.Persona{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          personaID,
		Name:        m.Name,
		Description: m.Description,
		AppID:       m.AppID,
		Identity:    m.Identity,
	}
	for _, f := range []struct {
		name string
		data string
		dest any
	}{
		{"skills", m.Skills, &p.Skills},
		{"traits", m.Traits, &p.Traits},
		{"behaviors", m.Behaviors, &p.Behaviors},
		{"cognitive_style", m.CognitiveStyle, &p.CognitiveStyle},
		{"communication_style", m.CommunicationStyle, &p.CommunicationStyle},
		{"perception", m.Perception, &p.Perception},
		{"metadata", m.Metadata, &p.Metadata},
	} {
		if err := unmarshalField(f.name, f.data, f.dest); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// ──────────────────────────────────────────────────
// Run model
// ──────────────────────────────────────────────────

type runModel struct {
	grove.BaseModel `grove:"table:cortex_runs"`
	ID              string     `grove:"id,pk"`
	AgentID         string     `grove:"agent_id,notnull"`
	TenantID        string     `grove:"tenant_id"`
	State           string     `grove:"state,notnull"`
	Input           string     `grove:"input"`
	Output          string     `grove:"output"`
	Error           string     `grove:"error"`
	StepCount       int        `grove:"step_count"`
	TokensUsed      int        `grove:"tokens_used"`
	StartedAt       *time.Time `grove:"started_at"`
	CompletedAt     *time.Time `grove:"completed_at"`
	PersonaRef      string     `grove:"persona_ref"`
	Metadata        string     `grove:"metadata"`
	CreatedAt       time.Time  `grove:"created_at"`
	UpdatedAt       time.Time  `grove:"updated_at"`
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

func runFromModel(m *runModel) (*run.Run, error) {
	runID, err := id.ParseAgentRunID(m.ID)
	if err != nil {
		return nil, err
	}
	agentID, err := id.ParseAgentID(m.AgentID)
	if err != nil {
		return nil, err
	}
	r := &run.Run{
		Entity:      cortex.Entity{CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt},
		ID:          runID,
		AgentID:     agentID,
		TenantID:    m.TenantID,
		State:       run.State(m.State),
		Input:       m.Input,
		Output:      m.Output,
		Error:       m.Error,
		StepCount:   m.StepCount,
		TokensUsed:  m.TokensUsed,
		StartedAt:   m.StartedAt,
		CompletedAt: m.CompletedAt,
		PersonaRef:  m.PersonaRef,
	}
	if err := unmarshalField("metadata", m.Metadata, &r.Metadata); err != nil {
		return nil, err
	}
	return r, nil
}

// ──────────────────────────────────────────────────
// Step model
// ──────────────────────────────────────────────────

type stepModel struct {
	grove.BaseModel `grove:"table:cortex_steps"`
	ID              string     `grove:"id,pk"`
	RunID           string     `grove:"run_id,notnull"`
	Index           int        `grove:"index,notnull"`
	Type            string     `grove:"type"`
	Input           string     `grove:"input"`
	Output          string     `grove:"output"`
	TokensUsed      int        `grove:"tokens_used"`
	StartedAt       *time.Time `grove:"started_at"`
	CompletedAt     *time.Time `grove:"completed_at"`
	Metadata        string     `grove:"metadata"`
	CreatedAt       time.Time  `grove:"created_at"`
	UpdatedAt       time.Time  `grove:"updated_at"`
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

func stepFromModel(m *stepModel) (*run.Step, error) {
	stepID, err := id.ParseStepID(m.ID)
	if err != nil {
		return nil, err
	}
	runID, err := id.ParseAgentRunID(m.RunID)
	if err != nil {
		return nil, err
	}
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
	if err := unmarshalField("metadata", m.Metadata, &s.Metadata); err != nil {
		return nil, err
	}
	return s, nil
}

// ──────────────────────────────────────────────────
// ToolCall model
// ──────────────────────────────────────────────────

type toolCallModel struct {
	grove.BaseModel `grove:"table:cortex_tool_calls"`
	ID              string     `grove:"id,pk"`
	StepID          string     `grove:"step_id,notnull"`
	RunID           string     `grove:"run_id,notnull"`
	ToolName        string     `grove:"tool_name,notnull"`
	Arguments       string     `grove:"arguments"`
	Result          string     `grove:"result"`
	Error           string     `grove:"error"`
	StartedAt       *time.Time `grove:"started_at"`
	CompletedAt     *time.Time `grove:"completed_at"`
	Metadata        string     `grove:"metadata"`
	CreatedAt       time.Time  `grove:"created_at"`
	UpdatedAt       time.Time  `grove:"updated_at"`
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
	if err := unmarshalField("metadata", m.Metadata, &tc.Metadata); err != nil {
		return nil, err
	}
	return tc, nil
}

// ──────────────────────────────────────────────────
// Memory model
// ──────────────────────────────────────────────────

type memoryModel struct {
	grove.BaseModel `grove:"table:cortex_memories"`
	ID              int64     `grove:"id,pk,autoincrement"`
	AgentID         string    `grove:"agent_id,notnull"`
	TenantID        string    `grove:"tenant_id"`
	Kind            string    `grove:"kind,notnull"`
	Key             string    `grove:"key"`
	Content         string    `grove:"content,notnull"`
	Metadata        string    `grove:"metadata"`
	CreatedAt       time.Time `grove:"created_at"`
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
	grove.BaseModel `grove:"table:cortex_checkpoints"`
	ID              string    `grove:"id,pk"`
	RunID           string    `grove:"run_id,notnull"`
	AgentID         string    `grove:"agent_id,notnull"`
	TenantID        string    `grove:"tenant_id"`
	Reason          string    `grove:"reason"`
	StepIndex       int       `grove:"step_index"`
	State           string    `grove:"state,notnull"`
	Decision        string    `grove:"decision"`
	Metadata        string    `grove:"metadata"`
	CreatedAt       time.Time `grove:"created_at"`
	UpdatedAt       time.Time `grove:"updated_at"`
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
	if err := unmarshalField("decision", m.Decision, &cp.Decision); err != nil {
		return nil, err
	}
	if err := unmarshalField("metadata", m.Metadata, &cp.Metadata); err != nil {
		return nil, err
	}
	return cp, nil
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
