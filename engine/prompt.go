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

	// ToolsRestricted says Tools is an explicit allowlist even when it
	// is empty, so tool resolution does not fall back to "every
	// registered tool". An overlay that withdraws the last tool an agent
	// had is the case this exists for: without it, a removal would read
	// as a grant.
	ToolsRestricted bool
}

// effectiveConfig layers the run settings four deep: engine defaults,
// then the agent's own config, then the overlays that apply to the run's
// scope, then the per-run overrides.
//
// The overlays arrive broadest scope first, so a narrower one assigns
// last and wins, matching the way its patches reach the prompt.
// Per-run overrides sit above every overlay because a caller naming a
// model for this one run means it, and an overlay is a standing
// preference rather than an instruction about this call.
func (e *Engine) effectiveConfig(ag *agent.Config, overrides *RunOverrides, overlays []*prompt.Overlay) resolvedConfig {
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

	for _, o := range overlays {
		if o.Model != "" {
			cfg.Model = o.Model
		}
		// Temperature and MaxTokens are honored whenever the pointer is
		// set, zero included. That is what the pointers on Overlay are
		// for: a host writing 0 is asking for 0, not leaving the field
		// alone, and quietly ignoring it would make the one value a host
		// cannot express the one it most obviously might want.
		if o.Temperature != nil {
			t := *o.Temperature
			cfg.Temperature = &t
		}
		if o.MaxTokens != nil {
			cfg.MaxTokens = *o.MaxTokens
		}
		if len(o.ToolsAdded) > 0 || len(o.ToolsRemoved) > 0 {
			// An agent that names no tools is allowed all of them, so
			// the implicit list is written out before a delta subtracts
			// from it. An empty list is only implicit until an overlay
			// has spoken: once one has, the emptiness is a decision, and
			// re-expanding it here would let the next overlay in this
			// same loop hand back everything a broader scope withdrew
			// while naming something else entirely.
			base := cfg.Tools
			if len(base) == 0 && !cfg.ToolsRestricted {
				base = e.registeredToolNames()
			}
			cfg.Tools = applyToolDelta(base, o.ToolsAdded, o.ToolsRemoved)
			cfg.ToolsRestricted = true
		}
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
		// A per-run tool list is an explicit allowlist, so it carries
		// the restriction with it. An empty override is "no override"
		// rather than "no tools", which is what it has always meant and
		// is the one reading that cannot widen anything.
		if len(overrides.Tools) > 0 {
			cfg.Tools = overrides.Tools
			cfg.ToolsRestricted = true
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
	overlays, err := e.loadScopeOverlays(ctx, ag.ID)
	if err != nil {
		return "", err
	}

	return e.buildSystemPrompt(ctx, ag, overrides, overlays)
}

// buildSystemPrompt is the half of BuildSystemPrompt that takes overlays
// already loaded, so a run can walk the scope once and spend the result
// on both its prompt and its config.
func (e *Engine) buildSystemPrompt(ctx context.Context, ag *agent.Config, overrides *RunOverrides, overlays []*prompt.Overlay) (string, error) {
	sections, err := e.collectSections(ctx, ag, overrides)
	if err != nil {
		return "", err
	}

	return prompt.Assemble(e.applyOverlaySections(sections, overlays)), nil
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

// loadScopeOverlays returns the overlays that apply to a run in the
// caller's scope, ordered broadest scope first.
//
// The ordering is the point, not a side effect. Both consumers of this
// slice apply what they find in the order it arrives, so a narrower
// scope has to come last to win. "Most specific wins" and "every
// overlay applies" only agree when nothing collides, and append-mode
// patches and tool deltas both collide routinely. The loop walks
// prefixes from shortest to longest, so the index IS the scope depth
// and broadest-first is produced here rather than inherited from
// whatever order a store returned rows in.
//
// Inheritance is this explicit ancestor walk, one exact lookup per
// prefix, and never a listing. Prefix matching in the stores widens
// DOWNWARD: listing overlays from [ws=A] hands back every project's
// overlay inside that workspace. Feeding those into one run would mix a
// sibling project's instructions and tool grants into a run that must
// never see them, and the call site would look perfectly reasonable
// while it happened.
//
// The walk starts at the first level rather than at the empty scope.
// CreateOverlay stamps an overlay with its creator's scope and WithScope
// refuses an empty one, so no overlay can be stored at the zero scope,
// and every backend answers a zero scope argument with
// ErrOverlayNotFound regardless.
//
// One walk serves both the prompt and the run config. Two traversals
// would be two orderings to keep in agreement, and they would drift the
// first time only one of them was edited.
func (e *Engine) loadScopeOverlays(ctx context.Context, agentID id.AgentID) ([]*prompt.Overlay, error) {
	if e.store == nil {
		return nil, nil
	}

	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() {
		return nil, nil
	}

	out := make([]*prompt.Overlay, 0, len(scope.Levels))

	for depth := 1; depth <= len(scope.Levels); depth++ {
		ancestor := cortex.Scope{Levels: scope.Levels[:depth]}

		o, err := e.store.GetOverlayForAgentAt(ctx, agentID, ancestor)
		if err != nil {
			if errors.Is(err, cortex.ErrOverlayNotFound) {
				continue
			}
			return nil, fmt.Errorf("load overlay at scope %q: %w", ancestor.Canonical(), err)
		}
		if o == nil {
			continue
		}
		out = append(out, o)
	}

	return out, nil
}

// applyOverlaySections patches the assembled sections with each
// overlay's patches, in the order loadScopeOverlays produced.
func (e *Engine) applyOverlaySections(sections []prompt.Section, overlays []*prompt.Overlay) []prompt.Section {
	for _, o := range overlays {
		if len(o.Patches) == 0 {
			continue
		}

		var declined []prompt.Patch
		sections, declined = prompt.ApplyOverlay(sections, o.Patches)
		e.logDeclinedPatches(o, declined)
	}

	return sections
}

// applyToolDelta applies one overlay's tool additions and removals.
//
// Removals run after additions, so a tool an overlay both grants and
// withdraws ends up withdrawn. An overlay naming the same tool in both
// lists is stating a restriction, and resolving that toward "granted"
// would hand out a tool the same document asked to take away.
//
// Across overlays the narrower scope simply comes last, so it can
// withdraw what a broader one granted and can re-grant what a broader
// one withdrew. That is the same "narrowest wins" rule the patches
// follow. It does mean a workspace-level removal is not a floor a
// project overlay is unable to lift, which is a real tradeoff: both
// overlays are written by the same host at scopes it controls, and a
// removal that outranked everything beneath it would be the only piece
// of an overlay that inherited downward instead of being overridden.
func applyToolDelta(tools, added, removed []string) []string {
	out := make([]string, 0, len(tools)+len(added))
	seen := make(map[string]struct{}, len(tools)+len(added))

	keep := func(name string) {
		if name == "" {
			return
		}
		if _, dup := seen[name]; dup {
			return
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}

	for _, t := range tools {
		keep(t)
	}
	for _, t := range added {
		keep(t)
	}

	if len(removed) == 0 {
		return out
	}

	drop := make(map[string]struct{}, len(removed))
	for _, t := range removed {
		drop[t] = struct{}{}
	}

	kept := make([]string, 0, len(out))
	for _, t := range out {
		if _, gone := drop[t]; gone {
			continue
		}
		kept = append(kept, t)
	}

	return kept
}

// logDeclinedPatches surfaces the patches ApplyOverlay refused, which is
// the whole reason it returns them separately. A replace against a
// locked section is dropped so the run can proceed, and so is a patch
// carrying a mode the prompt package does not implement. Either way the
// host whose patch silently did nothing has no other way to find out.
func (e *Engine) logDeclinedPatches(o *prompt.Overlay, declined []prompt.Patch) {
	if len(declined) == 0 {
		return
	}

	ids := make([]string, 0, len(declined))
	for _, p := range declined {
		ids = append(ids, p.ID)
	}

	e.logger.Warn("prompt overlay patches declined",
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
