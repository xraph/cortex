package api

// ── Agent requests ────────────────────────────────────

// CreateAgentRequest is the request body for creating an agent.
type CreateAgentRequest struct {
	Name            string         `json:"name" description:"Unique agent name"`
	Description     string         `json:"description,omitempty"`
	SystemPrompt    string         `json:"system_prompt" description:"Agent system prompt"`
	Model           string         `json:"model,omitempty" description:"LLM model (default: smart)"`
	Tools           []string       `json:"tools,omitempty" description:"Tool names (flat mode)"`
	MaxSteps        int            `json:"max_steps,omitempty"`
	MaxTokens       int            `json:"max_tokens,omitempty"`
	Temperature     float64        `json:"temperature,omitempty"`
	ReasoningLoop   string         `json:"reasoning_loop,omitempty"`
	PersonaRef      string         `json:"persona_ref,omitempty" description:"Persona name reference"`
	InlineSkills    []string       `json:"inline_skills,omitempty"`
	InlineTraits    []string       `json:"inline_traits,omitempty"`
	InlineBehaviors []string       `json:"inline_behaviors,omitempty"`
	Guardrails      map[string]any `json:"guardrails,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

// GetAgentRequest is the request for getting an agent by name.
type GetAgentRequest struct {
	Name string `path:"name" description:"Agent name"`
}

// ListAgentsRequest is the request for listing agents.
type ListAgentsRequest struct {
	Limit  int `query:"limit" description:"Max results (default: 50)"`
	Offset int `query:"offset" description:"Results to skip"`
}

// UpdateAgentRequest is the request body for updating an agent.
type UpdateAgentRequest struct {
	Name            string         `path:"name" description:"Agent name"`
	Description     string         `json:"description,omitempty"`
	SystemPrompt    string         `json:"system_prompt,omitempty"`
	Model           string         `json:"model,omitempty"`
	Tools           []string       `json:"tools,omitempty"`
	MaxSteps        int            `json:"max_steps,omitempty"`
	MaxTokens       int            `json:"max_tokens,omitempty"`
	Temperature     float64        `json:"temperature,omitempty"`
	ReasoningLoop   string         `json:"reasoning_loop,omitempty"`
	PersonaRef      string         `json:"persona_ref,omitempty"`
	InlineSkills    []string       `json:"inline_skills,omitempty"`
	InlineTraits    []string       `json:"inline_traits,omitempty"`
	InlineBehaviors []string       `json:"inline_behaviors,omitempty"`
	Guardrails      map[string]any `json:"guardrails,omitempty"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

// DeleteAgentRequest is the request for deleting an agent.
type DeleteAgentRequest struct {
	Name string `path:"name" description:"Agent name"`
}

// ── Run requests ──────────────────────────────────────

// RunAgentRequest is the request body for running an agent.
type RunAgentRequest struct {
	Name  string `path:"name" description:"Agent name"`
	Input string `json:"input" description:"User input"`
}

// StreamAgentRequest is the request body for streaming an agent run.
type StreamAgentRequest struct {
	Name  string `path:"name" description:"Agent name"`
	Input string `json:"input" description:"User input"`
}

// GetRunRequest is the request for getting a run by ID.
type GetRunRequest struct {
	RunID string `path:"id" description:"Run ID"`
}

// ListRunsRequest is the request for listing runs.
type ListRunsRequest struct {
	Limit  int `query:"limit"`
	Offset int `query:"offset"`
}

// CancelRunRequest is the request for cancelling a run.
type CancelRunRequest struct {
	RunID string `path:"id" description:"Run ID"`
}

// ── Skill requests ────────────────────────────────────

// CreateSkillRequest is the request body for creating a skill.
type CreateSkillRequest struct {
	Name                 string            `json:"name" description:"Unique skill name"`
	Description          string            `json:"description,omitempty"`
	Tools                []ToolBindingReq  `json:"tools,omitempty" description:"Tool bindings with mastery"`
	Knowledge            []KnowledgeRefReq `json:"knowledge,omitempty" description:"Knowledge sources"`
	SystemPromptFragment string            `json:"system_prompt_fragment,omitempty"`
	Dependencies         []string          `json:"dependencies,omitempty"`
	DefaultProficiency   string            `json:"default_proficiency,omitempty"`
	Metadata             map[string]any    `json:"metadata,omitempty"`
}

// ToolBindingReq is the request representation of a tool binding.
type ToolBindingReq struct {
	ToolName   string `json:"tool_name"`
	Mastery    string `json:"mastery,omitempty"`
	Guidance   string `json:"guidance,omitempty"`
	PreferWhen string `json:"prefer_when,omitempty"`
}

// KnowledgeRefReq is the request representation of a knowledge reference.
type KnowledgeRefReq struct {
	Source     string `json:"source"`
	InjectMode string `json:"inject_mode,omitempty"`
	Priority   int    `json:"priority,omitempty"`
}

// GetSkillRequest is the request for getting a skill by name.
type GetSkillRequest struct {
	Name string `path:"name" description:"Skill name"`
}

// ListSkillsRequest is the request for listing skills.
type ListSkillsRequest struct {
	Limit  int `query:"limit"`
	Offset int `query:"offset"`
}

// UpdateSkillRequest is the request body for updating a skill.
type UpdateSkillRequest struct {
	Name                 string            `path:"name" description:"Skill name"`
	Description          string            `json:"description,omitempty"`
	Tools                []ToolBindingReq  `json:"tools,omitempty"`
	Knowledge            []KnowledgeRefReq `json:"knowledge,omitempty"`
	SystemPromptFragment string            `json:"system_prompt_fragment,omitempty"`
	Dependencies         []string          `json:"dependencies,omitempty"`
	DefaultProficiency   string            `json:"default_proficiency,omitempty"`
	Metadata             map[string]any    `json:"metadata,omitempty"`
}

// DeleteSkillRequest is the request for deleting a skill.
type DeleteSkillRequest struct {
	Name string `path:"name" description:"Skill name"`
}

// ── Trait requests ────────────────────────────────────

// CreateTraitRequest is the request body for creating a trait.
type CreateTraitRequest struct {
	Name        string         `json:"name" description:"Unique trait name"`
	Description string         `json:"description,omitempty"`
	Dimensions  []DimensionReq `json:"dimensions,omitempty"`
	Influences  []InfluenceReq `json:"influences,omitempty"`
	Category    string         `json:"category,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// DimensionReq is the request representation of a personality dimension.
type DimensionReq struct {
	Name      string  `json:"name"`
	LowLabel  string  `json:"low_label"`
	HighLabel string  `json:"high_label"`
	Value     float64 `json:"value"`
}

// InfluenceReq is the request representation of a trait influence.
type InfluenceReq struct {
	Target    string  `json:"target"`
	Value     any     `json:"value"`
	Condition string  `json:"condition,omitempty"`
	Weight    float64 `json:"weight,omitempty"`
}

// GetTraitRequest is the request for getting a trait by name.
type GetTraitRequest struct {
	Name string `path:"name" description:"Trait name"`
}

// ListTraitsRequest is the request for listing traits.
type ListTraitsRequest struct {
	Limit    int    `query:"limit"`
	Offset   int    `query:"offset"`
	Category string `query:"category" description:"Filter by category"`
}

// UpdateTraitRequest is the request body for updating a trait.
type UpdateTraitRequest struct {
	Name        string         `path:"name" description:"Trait name"`
	Description string         `json:"description,omitempty"`
	Dimensions  []DimensionReq `json:"dimensions,omitempty"`
	Influences  []InfluenceReq `json:"influences,omitempty"`
	Category    string         `json:"category,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// DeleteTraitRequest is the request for deleting a trait.
type DeleteTraitRequest struct {
	Name string `path:"name" description:"Trait name"`
}

// ── Behavior requests ─────────────────────────────────

// CreateBehaviorRequest is the request body for creating a behavior.
type CreateBehaviorRequest struct {
	Name          string         `json:"name" description:"Unique behavior name"`
	Description   string         `json:"description,omitempty"`
	Triggers      []TriggerReq   `json:"triggers,omitempty"`
	Actions       []ActionReq    `json:"actions,omitempty"`
	Priority      int            `json:"priority,omitempty"`
	RequiresSkill string         `json:"requires_skill,omitempty"`
	RequiresTrait string         `json:"requires_trait,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// TriggerReq is the request representation of a behavior trigger.
type TriggerReq struct {
	Type    string `json:"type"`
	Pattern string `json:"pattern,omitempty"`
}

// ActionReq is the request representation of a behavior action.
type ActionReq struct {
	Type   string `json:"type"`
	Target string `json:"target,omitempty"`
	Value  any    `json:"value,omitempty"`
}

// GetBehaviorRequest is the request for getting a behavior by name.
type GetBehaviorRequest struct {
	Name string `path:"name" description:"Behavior name"`
}

// ListBehaviorsRequest is the request for listing behaviors.
type ListBehaviorsRequest struct {
	Limit  int `query:"limit"`
	Offset int `query:"offset"`
}

// UpdateBehaviorRequest is the request body for updating a behavior.
type UpdateBehaviorRequest struct {
	Name          string         `path:"name" description:"Behavior name"`
	Description   string         `json:"description,omitempty"`
	Triggers      []TriggerReq   `json:"triggers,omitempty"`
	Actions       []ActionReq    `json:"actions,omitempty"`
	Priority      int            `json:"priority,omitempty"`
	RequiresSkill string         `json:"requires_skill,omitempty"`
	RequiresTrait string         `json:"requires_trait,omitempty"`
	Metadata      map[string]any `json:"metadata,omitempty"`
}

// DeleteBehaviorRequest is the request for deleting a behavior.
type DeleteBehaviorRequest struct {
	Name string `path:"name" description:"Behavior name"`
}

// ── Persona requests ──────────────────────────────────

// CreatePersonaRequest is the request body for creating a persona.
type CreatePersonaRequest struct {
	Name               string                `json:"name" description:"Unique persona name"`
	Description        string                `json:"description,omitempty"`
	Identity           string                `json:"identity" description:"Self-description for system prompt"`
	Skills             []SkillAssignmentReq  `json:"skills,omitempty"`
	Traits             []TraitAssignmentReq  `json:"traits,omitempty"`
	Behaviors          []string              `json:"behaviors,omitempty"`
	CognitiveStyle     map[string]any        `json:"cognitive_style,omitempty"`
	CommunicationStyle map[string]any        `json:"communication_style,omitempty"`
	Perception         map[string]any        `json:"perception,omitempty"`
	Metadata           map[string]any        `json:"metadata,omitempty"`
}

// SkillAssignmentReq is the request representation of a skill assignment.
type SkillAssignmentReq struct {
	SkillName   string `json:"skill_name"`
	Proficiency string `json:"proficiency,omitempty"`
}

// TraitAssignmentReq is the request representation of a trait assignment.
type TraitAssignmentReq struct {
	TraitName       string             `json:"trait_name"`
	DimensionValues map[string]float64 `json:"dimension_values,omitempty"`
}

// GetPersonaRequest is the request for getting a persona by name.
type GetPersonaRequest struct {
	Name string `path:"name" description:"Persona name"`
}

// ListPersonasRequest is the request for listing personas.
type ListPersonasRequest struct {
	Limit  int `query:"limit"`
	Offset int `query:"offset"`
}

// UpdatePersonaRequest is the request body for updating a persona.
type UpdatePersonaRequest struct {
	Name               string                `path:"name" description:"Persona name"`
	Description        string                `json:"description,omitempty"`
	Identity           string                `json:"identity,omitempty"`
	Skills             []SkillAssignmentReq  `json:"skills,omitempty"`
	Traits             []TraitAssignmentReq  `json:"traits,omitempty"`
	Behaviors          []string              `json:"behaviors,omitempty"`
	CognitiveStyle     map[string]any        `json:"cognitive_style,omitempty"`
	CommunicationStyle map[string]any        `json:"communication_style,omitempty"`
	Perception         map[string]any        `json:"perception,omitempty"`
	Metadata           map[string]any        `json:"metadata,omitempty"`
}

// DeletePersonaRequest is the request for deleting a persona.
type DeletePersonaRequest struct {
	Name string `path:"name" description:"Persona name"`
}

// ── Checkpoint requests ───────────────────────────────

// ListCheckpointsRequest is the request for listing pending checkpoints.
type ListCheckpointsRequest struct {
	Limit  int `query:"limit"`
	Offset int `query:"offset"`
}

// ResolveCheckpointRequest is the request for resolving a checkpoint.
type ResolveCheckpointRequest struct {
	CheckpointID string `path:"id" description:"Checkpoint ID"`
	Decision     string `json:"decision" description:"approved or rejected"`
	DecidedBy    string `json:"decided_by,omitempty"`
}

// ── Memory requests ───────────────────────────────────

// GetConversationRequest is the request for getting conversation history.
type GetConversationRequest struct {
	Name  string `path:"name" description:"Agent name"`
	Limit int    `query:"limit"`
}

// ClearConversationRequest is the request for clearing conversation history.
type ClearConversationRequest struct {
	Name string `path:"name" description:"Agent name"`
}

// ── Tool requests ─────────────────────────────────────

// ListToolsRequest is the request for listing available tools.
type ListToolsRequest struct{}

// GetToolSchemaRequest is the request for getting a tool's schema.
type GetToolSchemaRequest struct {
	Name string `path:"name" description:"Tool name"`
}
