package engine

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/trait"
)

// Engine is the central coordinator for the Cortex agent system.
type Engine struct {
	config      cortex.Config
	logger      *slog.Logger
	store       store.Store
	extensions  *plugin.Registry
	pendingExts []plugin.Extension
}

// New creates a new Engine with the given options.
func New(opts ...Option) (*Engine, error) {
	e := &Engine{
		config: cortex.DefaultConfig(),
		logger: slog.Default(),
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

// Start initializes the engine for operation.
func (e *Engine) Start(ctx context.Context) error {
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

// ──────────────────────────────────────────────────
// Skill CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreateSkill(ctx context.Context, s *skill.Skill) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateSkill(ctx, s)
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

// ──────────────────────────────────────────────────
// Trait CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreateTrait(ctx context.Context, t *trait.Trait) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateTrait(ctx, t)
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

// ──────────────────────────────────────────────────
// Behavior CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreateBehavior(ctx context.Context, b *behavior.Behavior) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateBehavior(ctx, b)
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

// ──────────────────────────────────────────────────
// Persona CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreatePersona(ctx context.Context, p *persona.Persona) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreatePersona(ctx, p)
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

func (e *Engine) ListPendingCheckpoints(ctx context.Context, filter *checkpoint.ListFilter) ([]*checkpoint.Checkpoint, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListPending(ctx, filter)
}

func (e *Engine) ResolveCheckpoint(ctx context.Context, cpID id.CheckpointID, decision checkpoint.Decision) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.Resolve(ctx, cpID, decision)
}
