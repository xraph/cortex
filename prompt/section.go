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
type PatchMode string

const (
	// PatchReplace overwrites the target section's Body. It is the default
	// when a Patch omits Mode.
	PatchReplace PatchMode = "replace"

	// PatchAppend adds to the target section's Body rather than overwriting
	// it. It is the only mode a Locked section accepts.
	PatchAppend PatchMode = "append"
)

// Section is one addressable piece of a system prompt.
type Section struct {
	// ID identifies the section for patching. Stable across assemblies of
	// the same producer so an overlay can keep targeting it.
	ID string `json:"id"`

	// Title is prefixed to Body when assembling, if non-empty. Sections
	// emitted by the existing producers leave this empty so assembly stays
	// byte-identical to the prior single-string prompt.
	Title string `json:"title,omitempty"`

	// Body is the section's prompt text.
	Body string `json:"body"`

	// Order controls position in the assembled prompt. Lower runs first.
	Order int `json:"order"`

	// Locked marks a section a host pins as a guarantee to the platform.
	// A replace Patch against a locked section is declined; an append
	// Patch is still accepted.
	Locked bool `json:"locked,omitempty"`
}

// Patch describes a change an overlay wants applied to one section, matched
// by ID.
type Patch struct {
	// ID names the target section. An ID with no matching section becomes
	// a new section rather than an error, since a host patching a section
	// a persona did not emit is expressing intent, not making a mistake.
	ID string `json:"id"`

	// Body is the replacement or appended text, depending on Mode.
	Body string `json:"body"`

	// Mode selects how Body is applied. An empty Mode defaults to
	// PatchReplace.
	Mode PatchMode `json:"mode,omitempty"`
}

// ApplyOverlay applies patches to sections and returns the resulting
// section set. It copies sections rather than mutating the input, so a
// caller's slice is unaffected by the patches applied here.
//
// A replace Patch against a Locked section is declined: the run proceeds
// with the section unchanged rather than failing over a patch the platform
// pinned against. Declined patches are returned in the second value so the
// caller can surface them, since a patch that looks applied and silently
// is not has been a recurring source of confusion in prompt configuration.
// An append Patch is always accepted, locked or not. An unknown ID adds a
// new section instead of erroring.
func ApplyOverlay(sections []Section, patches []Patch) ([]Section, []Patch) {
	out := make([]Section, len(sections))
	copy(out, sections)

	index := make(map[string]int, len(out))
	for i, s := range out {
		index[s.ID] = i
	}

	var declined []Patch

	for _, p := range patches {
		mode := p.Mode
		if mode == "" {
			mode = PatchReplace
		}

		i, exists := index[p.ID]
		if !exists {
			index[p.ID] = len(out)
			out = append(out, Section{ID: p.ID, Body: p.Body})

			continue
		}

		if out[i].Locked && mode == PatchReplace {
			declined = append(declined, p)
			continue
		}

		if mode == PatchAppend {
			out[i].Body = appendBody(out[i].Body, p.Body)
		} else {
			out[i].Body = p.Body
		}
	}

	return out, declined
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
