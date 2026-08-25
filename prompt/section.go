// Package prompt models a system prompt as an ordered set of addressable
// sections rather than one opaque string. A host assembles sections from a
// persona, its skills and traits, then a tenant overlay can patch individual
// sections without being able to touch the ones the host pinned.
package prompt

import (
	"sort"
	"strings"
)

// PatchMode selects how a Patch applies to the section it targets.
// Only the two values below are implemented. A Patch carrying anything
// else names a mode this package cannot apply, and ApplyOverlay
// declines it rather than guessing which one was meant.
type PatchMode string

const (
	// PatchReplace overwrites the target section's Body. It is the default
	// when a Patch omits Mode.
	PatchReplace PatchMode = "replace"

	// PatchAppend adds to the target section's Body rather than overwriting
	// it. It is the only mode a Locked section accepts.
	PatchAppend PatchMode = "append"
)

// resolveMode maps a Patch's Mode onto a mode this package implements,
// reporting false for anything it does not recognize.
//
// The empty mode is the single alias, and it means replace, which is
// what every Patch written before Mode existed depends on. Nothing else
// is normalized. "Replace" and "REPLACE" are not accepted spellings of
// PatchReplace: a PatchMode is a bare string that arrives from stored
// JSON with nothing validating it on the way in, so a value that is not
// one of the two constants is as likely to be a typo as an intention,
// and treating it as replace is the one reading that can quietly
// overwrite text somebody pinned.
func resolveMode(m PatchMode) (PatchMode, bool) {
	switch m {
	case "":
		return PatchReplace, true
	case PatchReplace, PatchAppend:
		return m, true
	default:
		return "", false
	}
}

// Source names what produced a section. It is descriptive only: nothing
// in assembly branches on it, and it exists so an operator reading an
// assembled prompt can tell which part of the configuration to go and
// edit.
type Source string

const (
	// SourceHost marks a section the host wrote directly on the agent,
	// including the one an agent's plain SystemPrompt lowers into.
	SourceHost Source = "host"

	// SourcePersona marks a section produced by a persona.
	SourcePersona Source = "persona"

	// SourceSkill marks a section produced by a skill.
	SourceSkill Source = "skill"

	// SourceTrait marks a section produced by a trait.
	SourceTrait Source = "trait"

	// SourceKnowledge marks a section produced by retrieved knowledge.
	SourceKnowledge Source = "knowledge"
)

// Order bands place each producer's sections in the assembled prompt.
// The values reproduce the order the prompt has always had: the agent's
// own instructions, then the persona's identity, then one section per
// skill, then retrieved knowledge, then one section per trait injection.
//
// They are spaced a thousand apart for two reasons. A host can slot a
// section of its own between any two bands by picking a value in the
// gap, and a producer that emits several sections can take consecutive
// orders from its band without ever colliding with the next one.
const (
	OrderRole      = 1000
	OrderPersona   = 2000
	OrderSkill     = 3000
	OrderKnowledge = 4000
	OrderTrait     = 5000
)

// Section is one addressable piece of a system prompt.
type Section struct {
	// ID identifies the section for patching. Stable across assemblies of
	// the same producer so an overlay can keep targeting it.
	ID string `json:"id"`

	// Source names what produced this section. Purely informational.
	Source Source `json:"source,omitempty"`

	// Title is prefixed to Body when assembling, if non-empty. Sections
	// emitted by the existing producers leave this empty so assembly stays
	// byte-identical to the prior single-string prompt.
	Title string `json:"title,omitempty"`

	// Body is the section's prompt text.
	Body string `json:"body"`

	// Order controls position in the assembled prompt. Lower runs first.
	Order int `json:"order"`

	// Locked marks a section a host pins as a guarantee to the platform.
	// An append Patch is the only thing a locked section accepts.
	// Everything else is declined, replace and unrecognized modes both.
	//
	// The pin covers overlay patches and stops there. A caller who can
	// set a per-run system prompt discards the agent's stored sections
	// outright, the locked ones with them, and what that caller wrote
	// comes back as a single unlocked "role" section. So Locked pins a
	// preamble against the tenants who write overlays, not against the
	// callers who start runs. If you need it held against both, hold the
	// second one at whatever API boundary lets a caller set that field.
	Locked bool `json:"locked,omitempty"`
}

// Patch describes a change an overlay wants applied to one section, matched
// by ID.
type Patch struct {
	// ID names the target section. An ID with no matching section becomes
	// a new section rather than an error, since a host patching a section
	// a persona did not emit is expressing intent, not making a mistake.
	// A section created that way is placed after every section already
	// present, so an overlay cannot use an unrecognized ID to land text
	// ahead of a Locked one. Position stays a host decision: put the
	// section on the agent with the Order you want, then patch it by ID.
	ID string `json:"id"`

	// Body is the replacement or appended text, depending on Mode.
	Body string `json:"body"`

	// Mode selects how Body is applied. An empty Mode defaults to
	// PatchReplace. Any value other than those two is declined, on a
	// locked section and an unlocked one alike, so a mode that arrives
	// misspelled from a stored overlay changes nothing rather than
	// falling through to a replace nobody asked for.
	Mode PatchMode `json:"mode,omitempty"`
}

// ApplyOverlay applies patches to sections and returns the resulting
// section set. It copies sections rather than mutating the input, so a
// caller's slice is unaffected by the patches applied here.
//
// A Locked section accepts an append and nothing else. The rule is
// written that way round on purpose. Enumerating the modes to refuse
// leaves every value nobody thought of permitted, and PatchMode is a
// bare string persisted as JSON, so "Replace" or a typo would sail past
// a refusal list and overwrite the body a host pinned.
//
// A patch this package cannot apply is declined wherever it lands,
// locked section or not, and it never creates a section either. Reading
// an unrecognized mode as a replace would make every future mode name a
// destructive operation on today's build, which is the wrong way for
// that mistake to fail.
//
// A declined patch does not fail the run: assembly proceeds with the
// section as the host wrote it. The declined patches come back in the
// second value so the caller can surface them, since a patch that looks
// applied and silently is not has been a recurring source of confusion
// in prompt configuration.
//
// An unknown ID adds a new section, placed after every section already
// present so that a patch naming an ID nothing emitted cannot outrank a
// Locked one. Several creations in one call keep the order the patches
// were given in.
func ApplyOverlay(sections []Section, patches []Patch) ([]Section, []Patch) {
	out := make([]Section, len(sections))
	copy(out, sections)

	index := make(map[string]int, len(out))
	for i, s := range out {
		index[s.ID] = i
	}

	// Read the tail of the existing set once. Nothing below moves a
	// section that is already here, so the boundary does not shift.
	nextOrder := highestOrder(out) + 1

	var declined []Patch

	for _, p := range patches {
		// Resolve the mode before anything else so an unusable patch
		// cannot create a section on its way to being declined.
		mode, known := resolveMode(p.Mode)
		if !known {
			declined = append(declined, p)
			continue
		}

		i, exists := index[p.ID]
		if !exists {
			index[p.ID] = len(out)
			out = append(out, Section{ID: p.ID, Body: p.Body, Order: nextOrder})
			// One order per creation, so two new sections come out in
			// patch order rather than falling back to the ID tie-break.
			nextOrder++

			continue
		}

		if out[i].Locked && mode != PatchAppend {
			declined = append(declined, p)
			continue
		}

		switch mode {
		case PatchAppend:
			out[i].Body = appendBody(out[i].Body, p.Body)
		case PatchReplace:
			out[i].Body = p.Body
		}
	}

	return out, declined
}

// highestOrder reports the largest Order in sections, or zero when there
// are none. Zero is the right floor for an empty set: the producer bands
// all sit well above it, so a section created against no sections at all
// still sorts sanely once producers show up.
func highestOrder(sections []Section) int {
	highest := 0
	for _, s := range sections {
		if s.Order > highest {
			highest = s.Order
		}
	}

	return highest
}

func appendBody(existing, addition string) string {
	if existing == "" {
		return addition
	}
	// An append with nothing to add is a no-op, not a blank line. Without
	// this the separator still lands and the section grows a trailing
	// newline that survives into the assembled prompt.
	if addition == "" {
		return existing
	}

	return existing + "\n" + addition
}

// Assemble joins sections into a single prompt string, sorted by Order and
// then by ID for stability when two sections share an order. Sections are
// joined with a blank line between them. A section's Title is prefixed to
// its Body when the Title is non-empty; an empty Title produces no prefix
// and no stray blank line, which is the case every section without a
// host-authored Title hits.
func Assemble(sections []Section) string {
	sorted := make([]Section, len(sections))
	copy(sorted, sections)

	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Order != sorted[j].Order {
			return sorted[i].Order < sorted[j].Order
		}

		return sorted[i].ID < sorted[j].ID
	})

	parts := make([]string, 0, len(sorted))

	for _, s := range sorted {
		if s.Title == "" {
			parts = append(parts, s.Body)
		} else {
			parts = append(parts, s.Title+"\n"+s.Body)
		}
	}

	return strings.Join(parts, "\n\n")
}
