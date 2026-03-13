package engine

import (
	"context"
	"fmt"
	"time"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/trait"
)

// Engine is the central coordinator for the Cortex agent system.
type Engine struct {
	config      cortex.Config
	logger      log.Logger
	store       store.Store
	llm         llm.Client
	safety      safety.Scanner
	knowledge   knowledge.Provider
	extensions  *plugin.Registry
	pendingExts []plugin.Extension
}

// LLM returns the configured LLM client, or nil if none is set.
func (e *Engine) LLM() llm.Client { return e.llm }

// Safety returns the configured safety scanner, or nil if none is set.
func (e *Engine) Safety() safety.Scanner { return e.safety }

// Knowledge returns the configured knowledge provider, or nil if none is set.
func (e *Engine) Knowledge() knowledge.Provider { return e.knowledge }

// RunOverrides allows overriding agent configuration for a single run.
type RunOverrides struct {
	Model           string
	Temperature     *float64
	MaxSteps        int
	MaxTokens       int
	ReasoningLoop   string
	SystemPrompt    string
	PersonaRef      string
	InlineSkills    []string
	InlineTraits    []string
	InlineBehaviors []string
	Tools           []string
}

// New creates a new Engine with the given options.
func New(opts ...Option) (*Engine, error) {
	e := &Engine{
		config: cortex.DefaultConfig(),
		logger: log.NewNoopLogger(),
	}

	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, fmt.Errorf("cortex: apply engine option: %w", err)
		}
	}

	e.extensions = plugin.NewRegistry(e.logger)
	for _, ext := range e.pendingExts {
		e.extensions.Register(ext)
	}
	e.pendingExts = nil

	return e, nil
}

// Health checks the health of the engine by pinging its store.
func (e *Engine) Health(ctx context.Context) error {
	if e.store != nil {
		return e.store.Ping(ctx)
	}
	return nil
}

// Start initializes the engine for operation.
func (e *Engine) Start(_ context.Context) error {
	e.logger.Info("cortex engine started")
	return nil
}

// Stop gracefully shuts down the engine.
func (e *Engine) Stop(ctx context.Context) error {
	e.extensions.EmitShutdown(ctx)
	e.logger.Info("cortex engine stopped")
	return nil
}

// Store returns the composite store.
func (e *Engine) Store() store.Store { return e.store }

// Extensions returns the plugin registry.
func (e *Engine) Extensions() *plugin.Registry { return e.extensions }

// Config returns the engine configuration.
func (e *Engine) Config() cortex.Config { return e.config }

// UpdateConfig applies partial runtime configuration updates.
// Only non-zero fields in the update are applied. Changes are in-memory only.
func (e *Engine) UpdateConfig(update cortex.Config) cortex.Config {
	if update.DefaultModel != "" {
		e.config.DefaultModel = update.DefaultModel
	}
	if update.DefaultMaxSteps > 0 {
		e.config.DefaultMaxSteps = update.DefaultMaxSteps
	}
	if update.DefaultMaxTokens > 0 {
		e.config.DefaultMaxTokens = update.DefaultMaxTokens
	}
	if update.DefaultTemperature > 0 {
		e.config.DefaultTemperature = update.DefaultTemperature
	}
	if update.DefaultReasoningLoop != "" {
		e.config.DefaultReasoningLoop = update.DefaultReasoningLoop
	}
	return e.config
}

// ──────────────────────────────────────────────────
// Agent CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreateAgent(ctx context.Context, config *agent.Config) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.Create(ctx, config)
}

func (e *Engine) GetAgent(ctx context.Context, agentID id.AgentID) (*agent.Config, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.Get(ctx, agentID)
}

func (e *Engine) GetAgentByName(ctx context.Context, appID, name string) (*agent.Config, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetByName(ctx, appID, name)
}

func (e *Engine) UpdateAgent(ctx context.Context, config *agent.Config) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.Update(ctx, config)
}

func (e *Engine) DeleteAgent(ctx context.Context, agentID id.AgentID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.Delete(ctx, agentID)
}

func (e *Engine) ListAgents(ctx context.Context, filter *agent.ListFilter) ([]*agent.Config, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.List(ctx, filter)
}

func (e *Engine) CountAgents(ctx context.Context, filter *agent.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountAgents(ctx, filter)
}

// ──────────────────────────────────────────────────
// Skill CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreateSkill(ctx context.Context, s *skill.Skill) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateSkill(ctx, s)
}

func (e *Engine) GetSkill(ctx context.Context, skillID id.SkillID) (*skill.Skill, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetSkill(ctx, skillID)
}

func (e *Engine) GetSkillByName(ctx context.Context, appID, name string) (*skill.Skill, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetSkillByName(ctx, appID, name)
}

func (e *Engine) UpdateSkill(ctx context.Context, s *skill.Skill) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.UpdateSkill(ctx, s)
}

func (e *Engine) DeleteSkill(ctx context.Context, skillID id.SkillID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.DeleteSkill(ctx, skillID)
}

func (e *Engine) ListSkills(ctx context.Context, filter *skill.ListFilter) ([]*skill.Skill, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListSkills(ctx, filter)
}

func (e *Engine) CountSkills(ctx context.Context, filter *skill.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountSkills(ctx, filter)
}

// ──────────────────────────────────────────────────
// Trait CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreateTrait(ctx context.Context, t *trait.Trait) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateTrait(ctx, t)
}

func (e *Engine) GetTrait(ctx context.Context, traitID id.TraitID) (*trait.Trait, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetTrait(ctx, traitID)
}

func (e *Engine) GetTraitByName(ctx context.Context, appID, name string) (*trait.Trait, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetTraitByName(ctx, appID, name)
}

func (e *Engine) UpdateTrait(ctx context.Context, t *trait.Trait) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.UpdateTrait(ctx, t)
}

func (e *Engine) DeleteTrait(ctx context.Context, traitID id.TraitID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.DeleteTrait(ctx, traitID)
}

func (e *Engine) ListTraits(ctx context.Context, filter *trait.ListFilter) ([]*trait.Trait, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListTraits(ctx, filter)
}

func (e *Engine) CountTraits(ctx context.Context, filter *trait.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountTraits(ctx, filter)
}

// ──────────────────────────────────────────────────
// Behavior CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreateBehavior(ctx context.Context, b *behavior.Behavior) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateBehavior(ctx, b)
}

func (e *Engine) GetBehavior(ctx context.Context, behaviorID id.BehaviorID) (*behavior.Behavior, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetBehavior(ctx, behaviorID)
}

func (e *Engine) GetBehaviorByName(ctx context.Context, appID, name string) (*behavior.Behavior, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetBehaviorByName(ctx, appID, name)
}

func (e *Engine) UpdateBehavior(ctx context.Context, b *behavior.Behavior) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.UpdateBehavior(ctx, b)
}

func (e *Engine) DeleteBehavior(ctx context.Context, behaviorID id.BehaviorID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.DeleteBehavior(ctx, behaviorID)
}

func (e *Engine) ListBehaviors(ctx context.Context, filter *behavior.ListFilter) ([]*behavior.Behavior, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListBehaviors(ctx, filter)
}

func (e *Engine) CountBehaviors(ctx context.Context, filter *behavior.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountBehaviors(ctx, filter)
}

// ──────────────────────────────────────────────────
// Persona CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreatePersona(ctx context.Context, p *persona.Persona) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreatePersona(ctx, p)
}

func (e *Engine) GetPersona(ctx context.Context, personaID id.PersonaID) (*persona.Persona, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetPersona(ctx, personaID)
}

func (e *Engine) GetPersonaByName(ctx context.Context, appID, name string) (*persona.Persona, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetPersonaByName(ctx, appID, name)
}

func (e *Engine) UpdatePersona(ctx context.Context, p *persona.Persona) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.UpdatePersona(ctx, p)
}

func (e *Engine) DeletePersona(ctx context.Context, personaID id.PersonaID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.DeletePersona(ctx, personaID)
}

func (e *Engine) ListPersonas(ctx context.Context, filter *persona.ListFilter) ([]*persona.Persona, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListPersonas(ctx, filter)
}

func (e *Engine) CountPersonas(ctx context.Context, filter *persona.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountPersonas(ctx, filter)
}

// ──────────────────────────────────────────────────
// Run CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetRun(ctx, runID)
}

func (e *Engine) ListRuns(ctx context.Context, filter *run.ListFilter) ([]*run.Run, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListRuns(ctx, filter)
}

func (e *Engine) CountRuns(ctx context.Context, filter *run.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountRuns(ctx, filter)
}

func (e *Engine) ListSteps(ctx context.Context, runID id.AgentRunID) ([]*run.Step, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListSteps(ctx, runID)
}

func (e *Engine) ListToolCalls(ctx context.Context, stepID id.StepID) ([]*run.ToolCall, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListToolCalls(ctx, stepID)
}

// ──────────────────────────────────────────────────
// Memory passthrough
// ──────────────────────────────────────────────────

func (e *Engine) LoadConversation(ctx context.Context, agentID id.AgentID, tenantID string, limit int) ([]memory.Message, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.LoadConversation(ctx, agentID, tenantID, limit)
}

func (e *Engine) ClearConversation(ctx context.Context, agentID id.AgentID, tenantID string) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.ClearConversation(ctx, agentID, tenantID)
}

// ──────────────────────────────────────────────────
// Checkpoint passthrough
// ──────────────────────────────────────────────────

func (e *Engine) GetCheckpoint(ctx context.Context, cpID id.CheckpointID) (*checkpoint.Checkpoint, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetCheckpoint(ctx, cpID)
}

func (e *Engine) ListPendingCheckpoints(ctx context.Context, filter *checkpoint.ListFilter) ([]*checkpoint.Checkpoint, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListPending(ctx, filter)
}

func (e *Engine) CountPendingCheckpoints(ctx context.Context, filter *checkpoint.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountPending(ctx, filter)
}

func (e *Engine) ResolveCheckpoint(ctx context.Context, cpID id.CheckpointID, decision checkpoint.Decision) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.Resolve(ctx, cpID, decision)
}

// ──────────────────────────────────────────────────
// Agent execution
// ──────────────────────────────────────────────────

// StreamEventType identifies the kind of SSE event.
type StreamEventType string

const (
	EventRunStarted  StreamEventType = "run_started"
	EventStep        StreamEventType = "step"
	EventToolCall    StreamEventType = "tool_call"
	EventToken       StreamEventType = "token"
	EventCheckpoint  StreamEventType = "checkpoint"
	EventSafetyBlock StreamEventType = "safety_block"
	EventDone        StreamEventType = "done"
	EventError       StreamEventType = "error"
)

// StreamEvent is a single event emitted during streaming execution.
type StreamEvent struct {
	Type StreamEventType `json:"event"`
	Data map[string]any  `json:"data"`
}

// RunAgent executes an agent synchronously.
// When an LLM client is configured, it uses the ReAct reasoning loop.
// Otherwise, it falls back to mock/echo mode.
func (e *Engine) RunAgent(ctx context.Context, appID, agentName, input string, overrides *RunOverrides) (*run.Run, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}

	ag, err := e.store.GetByName(ctx, appID, agentName)
	if err != nil {
		return nil, fmt.Errorf("resolve agent: %w", err)
	}

	// Use real execution if LLM client is available.
	if e.llm != nil {
		return e.runReAct(ctx, ag, input, overrides)
	}

	// Fallback: mock/echo execution.
	return e.runMock(ctx, ag, input)
}

// StreamAgent executes an agent and sends streaming events to the channel.
// The channel is closed when execution completes.
// When an LLM client is configured, it uses the ReAct reasoning loop with streaming.
// Otherwise, it falls back to mock/echo mode.
func (e *Engine) StreamAgent(ctx context.Context, appID, agentName, input string, overrides *RunOverrides, events chan<- StreamEvent) error {
	if e.store == nil {
		close(events)
		return cortex.ErrNoStore
	}

	ag, err := e.store.GetByName(ctx, appID, agentName)
	if err != nil {
		close(events)
		return fmt.Errorf("resolve agent: %w", err)
	}

	// Use real execution if LLM client is available.
	if e.llm != nil {
		return e.streamReAct(ctx, ag, input, overrides, events)
	}

	// Fallback: mock/echo execution.
	return e.streamMock(ctx, ag, input, events)
}

// runMock is the mock/echo fallback for RunAgent.
func (e *Engine) runMock(ctx context.Context, ag *agent.Config, input string) (*run.Run, error) {
	now := time.Now().UTC()
	r := &run.Run{
		Entity:     cortex.NewEntity(),
		ID:         id.NewAgentRunID(),
		AgentID:    ag.ID,
		State:      run.StateRunning,
		Input:      input,
		StartedAt:  &now,
		PersonaRef: ag.PersonaRef,
	}
	if err := e.store.CreateRun(ctx, r); err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}

	e.extensions.EmitRunStarted(ctx, ag.ID, r.ID, input)

	stepStart := time.Now().UTC()
	step := &run.Step{
		Entity:     cortex.NewEntity(),
		ID:         id.NewStepID(),
		RunID:      r.ID,
		Index:      0,
		Type:       "generation",
		Input:      input,
		Output:     "Echo: " + input,
		TokensUsed: len(input) + 6,
		StartedAt:  &stepStart,
	}
	stepEnd := time.Now().UTC()
	step.CompletedAt = &stepEnd
	if err := e.store.CreateStep(ctx, step); err != nil {
		e.logger.Error("create step", log.String("error", err.Error()))
	}

	completedAt := time.Now().UTC()
	r.State = run.StateCompleted
	r.Output = step.Output
	r.StepCount = 1
	r.TokensUsed = step.TokensUsed
	r.CompletedAt = &completedAt
	if err := e.store.UpdateRun(ctx, r); err != nil {
		e.logger.Error("update run", log.String("error", err.Error()))
	}

	e.extensions.EmitRunCompleted(ctx, ag.ID, r.ID, r.Output, completedAt.Sub(now))

	return r, nil
}

// streamMock is the mock/echo fallback for StreamAgent.
func (e *Engine) streamMock(ctx context.Context, ag *agent.Config, input string, events chan<- StreamEvent) error {
	now := time.Now().UTC()
	r := &run.Run{
		Entity:     cortex.NewEntity(),
		ID:         id.NewAgentRunID(),
		AgentID:    ag.ID,
		State:      run.StateRunning,
		Input:      input,
		StartedAt:  &now,
		PersonaRef: ag.PersonaRef,
	}
	if err := e.store.CreateRun(ctx, r); err != nil {
		close(events)
		return fmt.Errorf("create run: %w", err)
	}

	e.extensions.EmitRunStarted(ctx, ag.ID, r.ID, input)

	go func() {
		defer close(events)

		events <- StreamEvent{Type: EventRunStarted, Data: map[string]any{
			"run_id":   r.ID.String(),
			"agent_id": ag.ID.String(),
		}}

		stepStart := time.Now().UTC()
		step := &run.Step{
			Entity:    cortex.NewEntity(),
			ID:        id.NewStepID(),
			RunID:     r.ID,
			Index:     0,
			Type:      "generation",
			Input:     input,
			StartedAt: &stepStart,
		}

		events <- StreamEvent{Type: EventStep, Data: map[string]any{
			"step_id": step.ID.String(),
			"index":   0,
			"type":    "generation",
		}}

		response := "Echo: " + input
		for i, ch := range response {
			select {
			case <-ctx.Done():
				r.State = run.StateCancelled
				completedAt := time.Now().UTC()
				r.CompletedAt = &completedAt
				if err := e.store.UpdateRun(ctx, r); err != nil {
					e.logger.Error("update run on cancel", log.String("error", err.Error()))
				}
				events <- StreamEvent{Type: EventError, Data: map[string]any{"message": "cancelled"}}
				return
			default:
			}

			events <- StreamEvent{Type: EventToken, Data: map[string]any{
				"content": string(ch),
				"index":   i,
			}}

			time.Sleep(20 * time.Millisecond)
		}

		stepEnd := time.Now().UTC()
		step.Output = response
		step.TokensUsed = len(input) + 6
		step.CompletedAt = &stepEnd
		if err := e.store.CreateStep(ctx, step); err != nil {
			e.logger.Error("create step", log.String("error", err.Error()))
		}

		completedAt := time.Now().UTC()
		r.State = run.StateCompleted
		r.Output = response
		r.StepCount = 1
		r.TokensUsed = step.TokensUsed
		r.CompletedAt = &completedAt
		if err := e.store.UpdateRun(ctx, r); err != nil {
			e.logger.Error("update run", log.String("error", err.Error()))
		}

		e.extensions.EmitRunCompleted(ctx, ag.ID, r.ID, r.Output, completedAt.Sub(now))

		events <- StreamEvent{Type: EventDone, Data: map[string]any{
			"run_id":      r.ID.String(),
			"output":      r.Output,
			"tokens_used": r.TokensUsed,
			"duration_ms": completedAt.Sub(now).Milliseconds(),
		}}
	}()

	return nil
}

// CreateRun creates a run record directly (for external use).
func (e *Engine) CreateRun(ctx context.Context, r *run.Run) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateRun(ctx, r)
}

// UpdateRun updates a run record (for external use like cancellation).
func (e *Engine) UpdateRun(ctx context.Context, r *run.Run) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.UpdateRun(ctx, r)
}
