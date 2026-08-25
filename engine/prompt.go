package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/skill"
)

// resolvedConfig holds the effective configuration after merging
// agent config, engine defaults, and per-run overrides.
type resolvedConfig struct {
	Model         string
	Temperature   *float64
	MaxSteps      int
	MaxTokens     int
	ReasoningLoop string
	Tools         []string
	PersonaRef    string
}

// effectiveConfig merges agent config + engine defaults + overrides.
// Priority: overrides > agent > engine defaults.
func (e *Engine) effectiveConfig(ag *agent.Config, overrides *RunOverrides) resolvedConfig {
	cfg := resolvedConfig{
		Model:         coalesceStr(ag.Model, e.config.DefaultModel),
		MaxSteps:      coalesceInt(ag.MaxSteps, e.config.DefaultMaxSteps),
		MaxTokens:     coalesceInt(ag.MaxTokens, e.config.DefaultMaxTokens),
		ReasoningLoop: coalesceStr(ag.ReasoningLoop, e.config.DefaultReasoningLoop),
		Tools:         ag.Tools,
		PersonaRef:    ag.PersonaRef,
	}

	// Agent temperature: use agent value if non-zero, otherwise engine default.
	if ag.Temperature != 0 {
		t := ag.Temperature
		cfg.Temperature = &t
	} else {
		t := e.config.DefaultTemperature
		cfg.Temperature = &t
	}

	// Apply overrides.
	if overrides != nil {
		if overrides.Model != "" {
			cfg.Model = overrides.Model
		}
		if overrides.MaxSteps > 0 {
			cfg.MaxSteps = overrides.MaxSteps
		}
		if overrides.MaxTokens > 0 {
			cfg.MaxTokens = overrides.MaxTokens
		}
		if overrides.ReasoningLoop != "" {
			cfg.ReasoningLoop = overrides.ReasoningLoop
		}
		if overrides.Temperature != nil {
			cfg.Temperature = overrides.Temperature
		}
		if len(overrides.Tools) > 0 {
			cfg.Tools = overrides.Tools
		}
		if overrides.PersonaRef != "" {
			cfg.PersonaRef = overrides.PersonaRef
		}
	}

	return cfg
}

// knowledgeTopK is how many chunks a skill's knowledge reference pulls
// into the prompt. It has been five since knowledge injection landed.
const knowledgeTopK = 5

// BuildSystemPrompt assembles the full system prompt for one run.
//
// It is a pipeline. Collect the sections the agent, its persona, its
// skills, the knowledge those skills reference and its traits produce;
// walk the run scope's ancestry and apply the overlay found at each
// rung, broadest first; assemble the result in Order.
//
// Persona, skill and trait lookups are scope-guarded: they resolve
// against cortex.ScopeFromContext(ctx), the same as the agent itself. A
// requested-but-unresolved persona, skill or trait fails the whole call
// loudly instead of dropping its fragment, because swallowing that error
// would let an agent quietly stop being itself with no signal anywhere.
// Section collection returns those errors rather than logging past them.
func (e *Engine) BuildSystemPrompt(ctx context.Context, ag *agent.Config, overrides *RunOverrides) (string, error) {
	sections, err := e.collectSections(ctx, ag, overrides)
	if err != nil {
		return "", err
	}

	sections, err = e.applyScopeOverlays(ctx, ag.ID, sections)
	if err != nil {
		return "", err
	}

	return prompt.Assemble(sections), nil
}

// collectSections gathers every producer's contribution in the order the
// prompt has always had: the agent's own instructions, the persona, one
// section per skill, retrieved knowledge, then one per trait injection.
func (e *Engine) collectSections(ctx context.Context, ag *agent.Config, overrides *RunOverrides) ([]prompt.Section, error) {
	sections := agentSections(ag, overrides)

	if e.store == nil {
		return sections, nil
	}

	personaRef := ag.PersonaRef
	if overrides != nil && overrides.PersonaRef != "" {
		personaRef = overrides.PersonaRef
	}

	skillNames := ag.InlineSkills
	if overrides != nil && len(overrides.InlineSkills) > 0 {
		skillNames = overrides.InlineSkills
	}

	traitNames := ag.InlineTraits
	if overrides != nil && len(overrides.InlineTraits) > 0 {
		traitNames = overrides.InlineTraits
	}

	// A non-empty personaRef is a request for a specific persona, not an
	// optional lookup, so a failure aborts instead of omitting Identity.
	if personaRef != "" {
		p, err := e.store.GetPersonaByName(ctx, personaRef)
		if err != nil {
			return nil, fmt.Errorf("resolve persona %q: %w", personaRef, err)
		}
		sections = append(sections, p.Sections(prompt.OrderPersona)...)
	}

	skills, err := e.resolveSkills(ctx, skillNames)
	if err != nil {
		return nil, err
	}

	// Each producer takes consecutive orders from its band so the
	// sections stay in the order the agent lists them. Assembly falls
	// back to sorting by ID when two sections share an order, which
	// would silently re-sort the skills alphabetically.
	order := prompt.OrderSkill
	for _, sk := range skills {
		secs := sk.Sections(order)
		order += len(secs)
		sections = append(sections, secs...)
	}

	sections = append(sections, e.knowledgeSections(ctx, skills)...)

	order = prompt.OrderTrait
	for _, name := range traitNames {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		t, tErr := e.store.GetTraitByName(ctx, name)
		if tErr != nil {
			return nil, fmt.Errorf("resolve trait %q: %w", name, tErr)
		}
		secs := t.Sections(order)
		order += len(secs)
		sections = append(sections, secs...)
	}

	return sections, nil
}

// agentSections returns the agent's own contribution.
//
// A per-run SystemPrompt override replaces that contribution outright,
// the agent's stored sections included, which is what overriding a
// prompt for one run has always meant. It arrives as the role section so
// an overlay targeting "role" still reaches it.
func agentSections(ag *agent.Config, overrides *RunOverrides) []prompt.Section {
	if overrides != nil && overrides.SystemPrompt != "" {
		return []prompt.Section{{
			ID:     agent.RoleSectionID,
			Source: prompt.SourceHost,
			Body:   overrides.SystemPrompt,
			Order:  prompt.OrderRole,
		}}
	}

	return ag.PromptSections()
}

// resolveSkills loads every named skill once. The prompt fragment and
// the knowledge references both come off the same record, and resolving
// twice let the second lookup fail silently where the first aborted.
func (e *Engine) resolveSkills(ctx context.Context, names []string) ([]*skill.Skill, error) {
	out := make([]*skill.Skill, 0, len(names))

	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		sk, err := e.store.GetSkillByName(ctx, name)
		if err != nil {
			return nil, fmt.Errorf("resolve skill %q: %w", name, err)
		}
		out = append(out, sk)
	}

	return out, nil
}

// knowledgeSections retrieves the knowledge a skill references.
//
// A retrieval failure skips that reference rather than failing the run,
// which is the behavior this had before sections existed: knowledge
// enriches a prompt, it does not define the agent, so an outage in the
// retrieval backend degrades the prompt instead of taking every agent
// that references a collection offline.
func (e *Engine) knowledgeSections(ctx context.Context, skills []*skill.Skill) []prompt.Section {
	if e.knowledge == nil {
		return nil
	}

	var out []prompt.Section
	order := prompt.OrderKnowledge
	seen := make(map[string]int)

	for _, sk := range skills {
		for _, ref := range sk.Knowledge {
			if ref.Source == "" {
				continue
			}
			chunks, err := e.knowledge.Retrieve(ctx, ref.Source, &knowledge.RetrieveParams{TopK: knowledgeTopK})
			if err != nil || len(chunks) == 0 {
				continue
			}

			seen[ref.Source]++
			sectionID := "knowledge:" + ref.Source
			if n := seen[ref.Source]; n > 1 {
				sectionID = fmt.Sprintf("knowledge:%s:%d", ref.Source, n)
			}

			out = append(out, prompt.Section{
				ID:     sectionID,
				Source: prompt.SourceKnowledge,
				Body:   knowledgeBody(ref.Source, chunks),
				Order:  order,
			})
			order++
		}
	}

	return out
}

// knowledgeBody renders retrieved chunks as the bulleted block this has
// always produced, minus one trailing newline.
//
// That newline is a deliberate change. The old block ended its last
// bullet with "\n" like every other bullet, and the old join then added
// its own separator on top, so a knowledge block was followed by three
// newlines where every other part had two. A section carries no
// trailing newline because assembly owns the separator, so a prompt with
// knowledge in it loses one blank line relative to v1.11.0. Keeping the
// newline instead would carry the extra blank line into every future
// assembly and make one section shaped unlike all the others.
func knowledgeBody(source string, chunks []knowledge.ScoredChunk) string {
	var b strings.Builder

	b.WriteString("## Knowledge: " + source)
	for _, c := range chunks {
		b.WriteString("\n- " + c.Content)
	}

	return b.String()
}

// applyScopeOverlays applies the overlay found at every rung of the run
// scope's own ancestry, broadest scope first.
//
// The ordering is the point, not a side effect. Patches are applied in
// the order the overlays arrive, so a narrower scope has to patch last
// to win. "Most specific wins" and "every overlay applies" only agree
// when patches never collide, and append mode makes collisions ordinary.
// The loop below walks prefixes from shortest to longest, so the index
// IS the scope depth and the broadest-first order is produced here
// rather than inherited from whatever order a store returned rows in.
//
// Inheritance is this explicit ancestor walk, one exact lookup per
// prefix, and never a listing. Prefix matching in the stores widens
// DOWNWARD: listing overlays from [ws=A] hands back every project's
// overlay inside that workspace. Feeding those into one run would
// assemble a sibling project's instructions into a prompt that must
// never see them, and the call site would look perfectly reasonable
// while it happened.
//
// The walk starts at the first level rather than at the empty scope.
// CreateOverlay stamps an overlay with its creator's scope and WithScope
// refuses an empty one, so no overlay can be stored at the zero scope,
// and every backend answers a zero scope argument with
// ErrOverlayNotFound regardless.
func (e *Engine) applyScopeOverlays(ctx context.Context, agentID id.AgentID, sections []prompt.Section) ([]prompt.Section, error) {
	if e.store == nil {
		return sections, nil
	}

	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return sections, nil
	}

	for depth := 1; depth <= len(scope.Levels); depth++ {
		ancestor := cortex.Scope{Levels: scope.Levels[:depth]}

		o, err := e.store.GetOverlayForAgentAt(ctx, agentID, ancestor)
		if err != nil {
			if errors.Is(err, cortex.ErrOverlayNotFound) {
				continue
			}
			return nil, fmt.Errorf("load overlay at scope %q: %w", ancestor.Canonical(), err)
		}
		if o == nil || len(o.Patches) == 0 {
			continue
		}

		var declined []prompt.Patch
		sections, declined = prompt.ApplyOverlay(sections, o.Patches)
		e.logDeclinedPatches(o, declined)
	}

	return sections, nil
}

// logDeclinedPatches surfaces the patches ApplyOverlay refused, which is
// the whole reason it returns them separately. A replace against a
// locked section is dropped so the run can proceed, and a host whose
// patch silently did nothing has no other way to find out.
func (e *Engine) logDeclinedPatches(o *prompt.Overlay, declined []prompt.Patch) {
	if len(declined) == 0 {
		return
	}

	ids := make([]string, 0, len(declined))
	for _, p := range declined {
		ids = append(ids, p.ID)
	}

	e.logger.Warn("prompt overlay patches declined by locked sections",
		log.String("overlay_id", o.ID.String()),
		log.String("agent_id", o.AgentID.String()),
		log.String("scope", o.Scope.Canonical()),
		log.String("sections", strings.Join(ids, ",")),
	)
}

// coalesceStr returns the first non-empty string.
func coalesceStr(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// coalesceInt returns the first non-zero int.
func coalesceInt(values ...int) int {
	for _, v := range values {
		if v > 0 {
			return v
		}
	}
	return 0
}
