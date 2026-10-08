package contract

import (
	"context"
	"fmt"
	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/trait"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"math"
	"reflect"
	"regexp"
	"slices"
	"strings"
)

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

func unit(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0 && v <= 1 }
func proficiency(v skill.Proficiency) bool {
	return slices.Contains([]skill.Proficiency{"", "novice", "apprentice", "competent", "proficient", "expert"}, v)
}
func (s *service) reference(ctx context.Context, kind, name string) error {
	if name == "" {
		return nil
	}
	var scope cortex.Scope
	var err error
	switch kind {
	case "agents":
		var v *agent.Config
		v, err = s.deps.Engine.GetAgentByName(ctx, name)
		if err == nil {
			scope = v.Scope
		}
	case "personas":
		var v *persona.Persona
		v, err = s.deps.Engine.GetPersonaByName(ctx, name)
		if err == nil {
			scope = v.Scope
		}
	case "skills":
		var v *skill.Skill
		v, err = s.deps.Engine.GetSkillByName(ctx, name)
		if err == nil {
			scope = v.Scope
		}
	case "traits":
		var v *trait.Trait
		v, err = s.deps.Engine.GetTraitByName(ctx, name)
		if err == nil {
			scope = v.Scope
		}
	case "behaviors":
		var v *behavior.Behavior
		v, err = s.deps.Engine.GetBehaviorByName(ctx, name)
		if err == nil {
			scope = v.Scope
		}
	}
	if err != nil {
		return bad(fmt.Sprintf("Cannot resolve %s reference %q in this scope", kind, name))
	}
	if scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		return bad("Composition references must belong to the same exact scope")
	}
	return nil
}
func (s *service) validate(ctx context.Context, v any) error {
	rv := reflect.ValueOf(v).Elem()
	if !validName.MatchString(rv.FieldByName("Name").String()) {
		return bad("Name must be 1 to 128 letters, numbers, dots, hyphens or underscores")
	}
	ref := func(kind, name string) error { return s.reference(ctx, kind, name) }
	switch x := v.(type) {
	case *agent.Config:
		if x.MaxSteps < 0 || x.MaxSteps > 1000 || x.MaxTokens < 0 || x.MaxTokens > 1000000 || math.IsNaN(x.Temperature) || x.Temperature < 0 || x.Temperature > 2 {
			return bad("Invalid agent step, token or temperature limit")
		}
		if x.ReasoningLoop != "" && x.ReasoningLoop != "react" {
			return bad("Only the ReAct reasoning loop is implemented by this engine")
		}
		if err := ref("personas", x.PersonaRef); err != nil {
			return err
		}
		for kind, names := range map[string][]string{"skills": x.InlineSkills, "traits": x.InlineTraits, "behaviors": x.InlineBehaviors} {
			for _, name := range names {
				if err := ref(kind, name); err != nil {
					return err
				}
			}
		}
		seen := map[string]bool{}
		for _, section := range x.Sections {
			if section.ID == "" || seen[section.ID] {
				return bad("Prompt section IDs must be nonempty and unique")
			}
			seen[section.ID] = true
		}
	case *persona.Persona:
		for _, r := range x.Skills {
			if !proficiency(r.Proficiency) {
				return bad("Unknown skill proficiency")
			}
			if err := ref("skills", r.SkillName); err != nil {
				return err
			}
		}
		for _, r := range x.Traits {
			if err := ref("traits", r.TraitName); err != nil {
				return err
			}
			for _, v := range r.DimensionValues {
				if !unit(v) {
					return bad("Trait dimension overrides must be between 0 and 1")
				}
			}
		}
		for _, name := range x.Behaviors {
			if err := ref("behaviors", name); err != nil {
				return err
			}
		}
		cs := x.CognitiveStyle
		if !unit(cs.DepthPreference) || !unit(cs.FocusPreference) || !unit(cs.ReflectionFrequency) {
			return bad("Cognitive preferences must be between 0 and 1")
		}
		for _, p := range cs.Phases {
			if !slices.Contains([]string{"analytical", "creative", "methodical", "reactive", "reflective", "collaborative"}, string(p.Strategy)) || p.MaxSteps < 0 {
				return bad("Invalid cognitive phase")
			}
		}
		comm := x.CommunicationStyle
		if !unit(comm.Formality) || !unit(comm.Verbosity) || !unit(comm.TechnicalLevel) || !unit(x.Perception.ContextWindow) || !unit(x.Perception.DetailOrientation) {
			return bad("Communication and perception preferences must be between 0 and 1")
		}
	case *skill.Skill:
		if !proficiency(x.DefaultProficiency) {
			return bad("Unknown skill proficiency")
		}
		for _, t := range x.Tools {
			if strings.TrimSpace(t.ToolName) == "" || !proficiency(t.Mastery) {
				return bad("Tool bindings need a name and supported mastery")
			}
		}
		for _, r := range x.Knowledge {
			if strings.TrimSpace(r.Source) == "" {
				return bad("Knowledge references need a source")
			}
		}
		for _, name := range x.Dependencies {
			if name == x.Name {
				return bad("A skill cannot depend on itself")
			}
			if err := ref("skills", name); err != nil {
				return err
			}
		}
	case *trait.Trait:
		if !slices.Contains([]string{"", "personality", "workstyle", "communication", "risk"}, string(x.Category)) {
			return bad("Unknown trait category")
		}
		for _, d := range x.Dimensions {
			if strings.TrimSpace(d.Name) == "" || !unit(d.Value) {
				return bad("Trait dimensions need a name and a value between 0 and 1")
			}
		}
		for _, i := range x.Influences {
			if !slices.Contains([]string{"prompt_injection", "temperature", "max_steps", "tool_selection", "response_style"}, string(i.Target)) || !unit(i.Weight) {
				return bad("Invalid trait influence")
			}
		}
	case *behavior.Behavior:
		if err := ref("skills", x.RequiresSkill); err != nil {
			return err
		}
		if err := ref("traits", x.RequiresTrait); err != nil {
			return err
		}
		for _, t := range x.Triggers {
			if !slices.Contains([]string{"on_input", "on_tool_result", "on_error", "on_step_count", "on_context", "always"}, string(t.Type)) {
				return bad("Unknown behavior trigger")
			}
		}
		for _, a := range x.Actions {
			if !slices.Contains([]string{"inject_prompt", "prefer_skill", "require_tool", "modify_param", "switch_cognitive", "add_guardrail"}, string(a.Type)) {
				return bad("Unknown behavior action")
			}
		}
	case *orchestration.Config:
		if !slices.Contains([]string{"sequential", "parallel", "router", "hierarchical", "debate"}, x.Strategy) || len(x.Participants) == 0 {
			return bad("Choose a supported strategy and at least one participant")
		}
		for _, p := range x.Participants {
			if err := ref("agents", p.AgentName); err != nil {
				return err
			}
		}
		for _, name := range []string{x.Settings.Manager, x.Settings.Judge, x.Settings.Aggregator, x.Settings.RouterAgent} {
			if err := ref("agents", name); err != nil {
				return err
			}
		}
		for _, name := range x.Settings.RouterRules {
			if err := ref("agents", name); err != nil {
				return err
			}
		}
		if x.Settings.Rounds < 0 || x.Settings.MaxConcurrency < 0 {
			return bad("Rounds and concurrency must be nonnegative")
		}
	}
	return nil
}

// Delete checks complete paged configuration reads, including descendants.
// External writers still need to coordinate: stores have no name foreign keys.
func (s *service) checkReferences(ctx context.Context, kind, name string) error {
	for offset := 0; ; offset += 100 {
		a, err := s.deps.Engine.ListAgents(ctx, &agent.ListFilter{Limit: 100, Offset: offset})
		if err != nil {
			return mapError(err)
		}
		p, err := s.deps.Engine.ListPersonas(ctx, &persona.ListFilter{Limit: 100, Offset: offset})
		if err != nil {
			return mapError(err)
		}
		b, err := s.deps.Engine.ListBehaviors(ctx, &behavior.ListFilter{Limit: 100, Offset: offset})
		if err != nil {
			return mapError(err)
		}
		sk, err := s.deps.Engine.ListSkills(ctx, &skill.ListFilter{Limit: 100, Offset: offset})
		if err != nil {
			return mapError(err)
		}
		o, err := s.deps.Engine.ListOrchestrations(ctx, &orchestration.ConfigListFilter{Limit: 100, Offset: offset})
		if err != nil {
			return mapError(err)
		}
		used := false
		for _, v := range a {
			switch kind {
			case "personas":
				used = used || v.PersonaRef == name
			case "skills":
				used = used || slices.Contains(v.InlineSkills, name)
			case "traits":
				used = used || slices.Contains(v.InlineTraits, name)
			case "behaviors":
				used = used || slices.Contains(v.InlineBehaviors, name)
			}
		}
		for _, v := range p {
			for _, r := range v.Skills {
				used = used || (kind == "skills" && r.SkillName == name)
			}
			for _, r := range v.Traits {
				used = used || (kind == "traits" && r.TraitName == name)
			}
			used = used || (kind == "behaviors" && slices.Contains(v.Behaviors, name))
		}
		for _, v := range b {
			used = used || (kind == "skills" && v.RequiresSkill == name) || (kind == "traits" && v.RequiresTrait == name)
		}
		for _, v := range sk {
			used = used || (kind == "skills" && slices.Contains(v.Dependencies, name))
		}
		if kind == "agents" {
			for _, v := range o {
				for _, p := range v.Participants {
					used = used || p.AgentName == name
				}
				for _, n := range []string{v.Settings.Manager, v.Settings.Judge, v.Settings.Aggregator, v.Settings.RouterAgent} {
					used = used || n == name
				}
				for _, n := range v.Settings.RouterRules {
					used = used || n == name
				}
			}
		}
		if used {
			return &dc.Error{Code: dc.CodeConflict, Message: "This configuration is still referenced. Remove its composition references first."}
		}
		if len(a) < 100 && len(p) < 100 && len(b) < 100 && len(sk) < 100 && len(o) < 100 {
			return nil
		}
	}
}
