package store

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/trait"
)

// PreparationSources is the read-only source set consumed by preparation.
// A snapshot view is scoped to its capture context and expires when its callback
// returns. Callers must not retain the view or use it outside that callback.
type PreparationSources interface {
	Get(context.Context, id.AgentID) (*agent.Config, error)
	GetPersonaByName(context.Context, string) (*persona.Persona, error)
	GetSkillByName(context.Context, string) (*skill.Skill, error)
	GetTraitByName(context.Context, string) (*trait.Trait, error)
	GetOverlayForAgentAt(context.Context, id.AgentID, cortex.Scope) (*prompt.Overlay, error)
}

// PreparationSnapshotter is an optional store capability. CapturePreparation
// calls collect exactly once with a transactionally consistent, read-only view.
// All reads, including absent overlays, share one snapshot. It returns a receipt
// only after successful collection and transaction completion. It never retries
// collect or falls back to ordinary reads. External knowledge is outside this
// contract. Implementations must preserve cancellation and exact caller scope.
type PreparationSnapshotter interface {
	CapturePreparation(ctx context.Context, collect func(PreparationSources) error) (*run.SourceSnapshot, error)
}
