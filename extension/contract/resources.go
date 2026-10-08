package contract

import (
	"context"
	"fmt"
	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/communication"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/perception"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/trait"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"
	"reflect"
)

type AgentPatch struct {
	Description     *string           `json:"description,omitempty"`
	SystemPrompt    *string           `json:"system_prompt,omitempty"`
	Model           *string           `json:"model,omitempty"`
	Tools           *[]string         `json:"tools,omitempty"`
	MaxSteps        *int              `json:"max_steps,omitempty"`
	MaxTokens       *int              `json:"max_tokens,omitempty"`
	Temperature     *float64          `json:"temperature,omitempty"`
	ReasoningLoop   *string           `json:"reasoning_loop,omitempty"`
	Guardrails      *map[string]any   `json:"guardrails,omitempty"`
	Metadata        *map[string]any   `json:"metadata,omitempty"`
	Enabled         *bool             `json:"enabled,omitempty"`
	PersonaRef      *string           `json:"persona_ref,omitempty"`
	InlineSkills    *[]string         `json:"inline_skills,omitempty"`
	InlineTraits    *[]string         `json:"inline_traits,omitempty"`
	InlineBehaviors *[]string         `json:"inline_behaviors,omitempty"`
	Sections        *[]prompt.Section `json:"sections,omitempty"`
}

func (s *service) bindAgent(d *dispatcher.Dispatcher) error {
	return bindResource[agent.Config, AgentPatch](d, s, "agents", resourceOps[agent.Config]{
		list: func(ctx context.Context, in ListInput) ([]*agent.Config, int64, error) {
			f := &agent.ListFilter{Search: in.Search, Limit: in.Limit, Offset: in.Offset, Exact: in.Exact}
			rows, err := s.deps.Engine.Store().List(ctx, f)
			if err != nil {
				return nil, 0, err
			}
			total, err := s.deps.Engine.Store().CountAgents(ctx, f)
			return rows, total, err
		},
		get: func(ctx context.Context, raw string) (*agent.Config, error) {
			v, err := id.ParseAgentID(raw)
			if err != nil {
				return nil, bad("Invalid agent ID")
			}
			return s.deps.Engine.Store().Get(ctx, v)
		},
		create: func(ctx context.Context, v *agent.Config) error {
			return s.deps.Engine.CreateAgent(ctx, v)
		},
		update: s.deps.Engine.UpdateAgent,
		remove: func(ctx context.Context, v *agent.Config) error { return s.deps.Engine.DeleteAgent(ctx, v.ID) },
		scope:  func(v *agent.Config) cortex.Scope { return v.Scope },
		name:   func(v *agent.Config) string { return v.Name },
	})
}

type PersonaPatch struct {
	Description        *string                    `json:"description,omitempty"`
	Identity           *string                    `json:"identity,omitempty"`
	Skills             *[]persona.SkillAssignment `json:"skills,omitempty"`
	Traits             *[]persona.TraitAssignment `json:"traits,omitempty"`
	Behaviors          *[]string                  `json:"behaviors,omitempty"`
	CognitiveStyle     *cognitive.Style           `json:"cognitive_style,omitempty"`
	CommunicationStyle *communication.Style       `json:"communication_style,omitempty"`
	Perception         *perception.Model          `json:"perception,omitempty"`
	Metadata           *map[string]any            `json:"metadata,omitempty"`
}

func (s *service) bindPersona(d *dispatcher.Dispatcher) error {
	return bindResource[persona.Persona, PersonaPatch](d, s, "personas", resourceOps[persona.Persona]{
		list: func(ctx context.Context, in ListInput) ([]*persona.Persona, int64, error) {
			f := &persona.ListFilter{Search: in.Search, Limit: in.Limit, Offset: in.Offset, Exact: in.Exact}
			rows, err := s.deps.Engine.Store().ListPersonas(ctx, f)
			if err != nil {
				return nil, 0, err
			}
			total, err := s.deps.Engine.Store().CountPersonas(ctx, f)
			return rows, total, err
		},
		get: func(ctx context.Context, raw string) (*persona.Persona, error) {
			v, err := id.ParsePersonaID(raw)
			if err != nil {
				return nil, bad("Invalid persona ID")
			}
			return s.deps.Engine.Store().GetPersona(ctx, v)
		},
		create: func(ctx context.Context, v *persona.Persona) error {
			return s.deps.Engine.CreatePersona(ctx, v)
		},
		update: s.deps.Engine.UpdatePersona,
		remove: func(ctx context.Context, v *persona.Persona) error { return s.deps.Engine.DeletePersona(ctx, v.ID) },
		scope:  func(v *persona.Persona) cortex.Scope { return v.Scope },
		name:   func(v *persona.Persona) string { return v.Name },
	})
}

type SkillPatch struct {
	Description          *string               `json:"description,omitempty"`
	Tools                *[]skill.ToolBinding  `json:"tools,omitempty"`
	Knowledge            *[]skill.KnowledgeRef `json:"knowledge,omitempty"`
	SystemPromptFragment *string               `json:"system_prompt_fragment,omitempty"`
	Dependencies         *[]string             `json:"dependencies,omitempty"`
	DefaultProficiency   *skill.Proficiency    `json:"default_proficiency,omitempty"`
	Metadata             *map[string]any       `json:"metadata,omitempty"`
}

func (s *service) bindSkill(d *dispatcher.Dispatcher) error {
	return bindResource[skill.Skill, SkillPatch](d, s, "skills", resourceOps[skill.Skill]{
		list: func(ctx context.Context, in ListInput) ([]*skill.Skill, int64, error) {
			f := &skill.ListFilter{Search: in.Search, Limit: in.Limit, Offset: in.Offset, Exact: in.Exact}
			rows, err := s.deps.Engine.Store().ListSkills(ctx, f)
			if err != nil {
				return nil, 0, err
			}
			total, err := s.deps.Engine.Store().CountSkills(ctx, f)
			return rows, total, err
		},
		get: func(ctx context.Context, raw string) (*skill.Skill, error) {
			v, err := id.ParseSkillID(raw)
			if err != nil {
				return nil, bad("Invalid skill ID")
			}
			return s.deps.Engine.Store().GetSkill(ctx, v)
		},
		create: func(ctx context.Context, v *skill.Skill) error {
			return s.deps.Engine.CreateSkill(ctx, v)
		},
		update: s.deps.Engine.UpdateSkill,
		remove: func(ctx context.Context, v *skill.Skill) error { return s.deps.Engine.DeleteSkill(ctx, v.ID) },
		scope:  func(v *skill.Skill) cortex.Scope { return v.Scope },
		name:   func(v *skill.Skill) string { return v.Name },
	})
}

type TraitPatch struct {
	Description *string            `json:"description,omitempty"`
	Dimensions  *[]trait.Dimension `json:"dimensions,omitempty"`
	Influences  *[]trait.Influence `json:"influences,omitempty"`
	Category    *trait.Category    `json:"category,omitempty"`
	Metadata    *map[string]any    `json:"metadata,omitempty"`
}

func (s *service) bindTrait(d *dispatcher.Dispatcher) error {
	return bindResource[trait.Trait, TraitPatch](d, s, "traits", resourceOps[trait.Trait]{
		list: func(ctx context.Context, in ListInput) ([]*trait.Trait, int64, error) {
			f := &trait.ListFilter{Search: in.Search, Limit: in.Limit, Offset: in.Offset, Exact: in.Exact}
			rows, err := s.deps.Engine.Store().ListTraits(ctx, f)
			if err != nil {
				return nil, 0, err
			}
			total, err := s.deps.Engine.Store().CountTraits(ctx, f)
			return rows, total, err
		},
		get: func(ctx context.Context, raw string) (*trait.Trait, error) {
			v, err := id.ParseTraitID(raw)
			if err != nil {
				return nil, bad("Invalid trait ID")
			}
			return s.deps.Engine.Store().GetTrait(ctx, v)
		},
		create: func(ctx context.Context, v *trait.Trait) error {
			return s.deps.Engine.CreateTrait(ctx, v)
		},
		update: s.deps.Engine.UpdateTrait,
		remove: func(ctx context.Context, v *trait.Trait) error { return s.deps.Engine.DeleteTrait(ctx, v.ID) },
		scope:  func(v *trait.Trait) cortex.Scope { return v.Scope },
		name:   func(v *trait.Trait) string { return v.Name },
	})
}

type BehaviorPatch struct {
	Description   *string             `json:"description,omitempty"`
	Triggers      *[]behavior.Trigger `json:"triggers,omitempty"`
	Actions       *[]behavior.Action  `json:"actions,omitempty"`
	Priority      *int                `json:"priority,omitempty"`
	RequiresSkill *string             `json:"requires_skill,omitempty"`
	RequiresTrait *string             `json:"requires_trait,omitempty"`
	Metadata      *map[string]any     `json:"metadata,omitempty"`
}

func (s *service) bindBehavior(d *dispatcher.Dispatcher) error {
	return bindResource[behavior.Behavior, BehaviorPatch](d, s, "behaviors", resourceOps[behavior.Behavior]{
		list: func(ctx context.Context, in ListInput) ([]*behavior.Behavior, int64, error) {
			f := &behavior.ListFilter{Search: in.Search, Limit: in.Limit, Offset: in.Offset, Exact: in.Exact}
			rows, err := s.deps.Engine.Store().ListBehaviors(ctx, f)
			if err != nil {
				return nil, 0, err
			}
			total, err := s.deps.Engine.Store().CountBehaviors(ctx, f)
			return rows, total, err
		},
		get: func(ctx context.Context, raw string) (*behavior.Behavior, error) {
			v, err := id.ParseBehaviorID(raw)
			if err != nil {
				return nil, bad("Invalid behavior ID")
			}
			return s.deps.Engine.Store().GetBehavior(ctx, v)
		},
		create: func(ctx context.Context, v *behavior.Behavior) error {
			return s.deps.Engine.CreateBehavior(ctx, v)
		},
		update: s.deps.Engine.UpdateBehavior,
		remove: func(ctx context.Context, v *behavior.Behavior) error { return s.deps.Engine.DeleteBehavior(ctx, v.ID) },
		scope:  func(v *behavior.Behavior) cortex.Scope { return v.Scope },
		name:   func(v *behavior.Behavior) string { return v.Name },
	})
}

type OrchestrationPatch struct {
	Description  *string                      `json:"description,omitempty"`
	Strategy     *string                      `json:"strategy,omitempty"`
	Participants *[]orchestration.Participant `json:"participants,omitempty"`
	Settings     *orchestration.Settings      `json:"settings,omitempty"`
	Metadata     *map[string]any              `json:"metadata,omitempty"`
}

func (s *service) bindOrchestration(d *dispatcher.Dispatcher) error {
	return bindResource[orchestration.Config, OrchestrationPatch](d, s, "orchestrations", resourceOps[orchestration.Config]{
		list: func(ctx context.Context, in ListInput) ([]*orchestration.Config, int64, error) {
			f := &orchestration.ConfigListFilter{Search: in.Search, Limit: in.Limit, Offset: in.Offset, Exact: in.Exact}
			rows, err := s.deps.Engine.Store().ListOrchestrations(ctx, f)
			if err != nil {
				return nil, 0, err
			}
			total, err := s.deps.Engine.Store().CountOrchestrations(ctx, f)
			return rows, total, err
		},
		get: func(ctx context.Context, raw string) (*orchestration.Config, error) {
			v, err := id.ParseOrchestrationConfigID(raw)
			if err != nil {
				return nil, bad("Invalid orchestration ID")
			}
			return s.deps.Engine.Store().GetOrchestration(ctx, v)
		},
		create: func(ctx context.Context, v *orchestration.Config) error {
			return s.deps.Engine.CreateOrchestration(ctx, v)
		},
		update: s.deps.Engine.UpdateOrchestration,
		remove: func(ctx context.Context, v *orchestration.Config) error {
			return s.deps.Engine.DeleteOrchestration(ctx, v.ID)
		},
		scope: func(v *orchestration.Config) cortex.Scope { return v.Scope },
		name:  func(v *orchestration.Config) string { return v.Name },
	})
}

type ListInput struct {
	Search string `json:"search"`
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
	Exact  bool   `json:"exact"`
}

func (in *ListInput) normalize() error {
	if in.Limit == 0 {
		in.Limit = 25
	}
	if in.Limit < 1 || in.Limit > 100 || in.Offset < 0 {
		return bad("Limit must be 1 to 100 and offset must be nonnegative")
	}
	return nil
}

type IDInput struct {
	ID string `json:"id"`
}
type ListOutput[T any] struct {
	Items  []*T  `json:"items"`
	Total  int64 `json:"total"`
	Limit  int   `json:"limit"`
	Offset int   `json:"offset"`
}
type CreateInput[T any] struct {
	Data T `json:"data"`
}
type UpdateInput[P any] struct {
	ID    string `json:"id"`
	Patch P      `json:"patch"`
}
type resourceOps[T any] struct {
	list   func(context.Context, ListInput) ([]*T, int64, error)
	get    func(context.Context, string) (*T, error)
	create func(context.Context, *T) error
	update func(context.Context, *T) error
	remove func(context.Context, *T) error
	scope  func(*T) cortex.Scope
	name   func(*T) string
}

func applyPatch[T, P any](value *T, patch P) {
	src, dst := reflect.ValueOf(patch), reflect.ValueOf(value).Elem()
	for i := 0; i < src.NumField(); i++ {
		if !src.Field(i).IsNil() {
			dst.FieldByName(src.Type().Field(i).Name).Set(src.Field(i).Elem())
		}
	}
}
func bindResource[T, P any](d *dispatcher.Dispatcher, s *service, name string, ops resourceOps[T]) error {
	if err := dispatcher.RegisterQuery(d, ContributorName, name+".list", 1, func(ctx context.Context, in ListInput, p dc.Principal) (ListOutput[T], error) {
		var out ListOutput[T]
		ctx, err := s.context(ctx, p, "read")
		if err != nil {
			return out, err
		}
		if err = in.normalize(); err != nil {
			return out, err
		}
		rows, total, err := ops.list(ctx, in)
		if rows == nil {
			rows = []*T{}
		}
		return ListOutput[T]{rows, total, in.Limit, in.Offset}, mapError(err)
	}); err != nil {
		return err
	}
	if err := dispatcher.RegisterQuery(d, ContributorName, name+".detail", 1, func(ctx context.Context, in IDInput, p dc.Principal) (*T, error) {
		ctx, err := s.context(ctx, p, "read")
		if err != nil {
			return nil, err
		}
		v, err := ops.get(ctx, in.ID)
		return v, mapError(err)
	}); err != nil {
		return err
	}
	if err := dispatcher.RegisterCommand(d, ContributorName, name+".create", 1, func(ctx context.Context, in CreateInput[T], p dc.Principal) (*T, error) {
		ctx, err := s.context(ctx, p, "manage")
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		prepareResource(ctx, &in.Data)
		if err = s.validate(ctx, &in.Data); err != nil {
			return nil, mapError(err)
		}
		err = s.write(ctx, p, name+".create", resourceID(&in.Data), func() error { return ops.create(ctx, &in.Data) })
		return &in.Data, err
	}); err != nil {
		return err
	}
	if err := dispatcher.RegisterCommand(d, ContributorName, name+".update", 1, func(ctx context.Context, in UpdateInput[P], p dc.Principal) (*T, error) {
		ctx, err := s.context(ctx, p, "manage")
		if err != nil {
			return nil, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		v, err := ops.get(ctx, in.ID)
		if err != nil {
			return nil, mapError(err)
		}
		ctx = cortex.WithScope(ctx, ops.scope(v))
		applyPatch(v, in.Patch)
		if err = s.validate(ctx, v); err != nil {
			return nil, mapError(err)
		}
		err = s.write(ctx, p, name+".update", in.ID, func() error { return ops.update(ctx, v) })
		return v, err
	}); err != nil {
		return err
	}
	return dispatcher.RegisterCommand(d, ContributorName, name+".delete", 1, func(ctx context.Context, in IDInput, p dc.Principal) (struct{}, error) {
		ctx, err := s.context(ctx, p, "manage")
		if err != nil {
			return struct{}{}, err
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		v, err := ops.get(ctx, in.ID)
		if err != nil {
			return struct{}{}, mapError(err)
		}
		if err = s.checkReferences(cortex.WithScope(ctx, ops.scope(v)), name, ops.name(v)); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, s.write(ctx, p, name+".delete", in.ID, func() error { return ops.remove(ctx, v) })
	})
}

func resourceID(v any) string {
	return reflect.ValueOf(v).Elem().FieldByName("ID").Interface().(fmt.Stringer).String()
}
func prepareResource(ctx context.Context, v any) {
	switch x := v.(type) {
	case *agent.Config:
		x.Entity = cortex.NewEntity()
		x.ID = id.NewAgentID()
		x.Scope = cortex.ScopeFromContext(ctx)
	case *persona.Persona:
		x.Entity = cortex.NewEntity()
		x.ID = id.NewPersonaID()
		x.Scope = cortex.ScopeFromContext(ctx)
	case *skill.Skill:
		x.Entity = cortex.NewEntity()
		x.ID = id.NewSkillID()
		x.Scope = cortex.ScopeFromContext(ctx)
	case *trait.Trait:
		x.Entity = cortex.NewEntity()
		x.ID = id.NewTraitID()
		x.Scope = cortex.ScopeFromContext(ctx)
	case *behavior.Behavior:
		x.Entity = cortex.NewEntity()
		x.ID = id.NewBehaviorID()
		x.Scope = cortex.ScopeFromContext(ctx)
	case *orchestration.Config:
		x.Entity = cortex.NewEntity()
		x.ID = id.NewOrchestrationConfigID()
		x.Scope = cortex.ScopeFromContext(ctx)
	}
}
