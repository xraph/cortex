package engine

import (
	"fmt"
	"math"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/prompt"
	"github.com/xraph/cortex/run"
)

// PreparedConfig describes frozen settings. Inspection returns detached data.
type PreparedConfig resolvedConfig

// PreparedView is a detached view for trusted host inspection and edit policy.
// Changing it cannot alter its originating snapshot. Tool schema numbers use
// json.Number to preserve integers beyond float64 precision.
type PreparedView struct {
	AgentID    id.AgentID
	Scope      cortex.Scope
	Config     PreparedConfig
	Sections   []prompt.Section
	Tools      []llm.Tool
	Provenance run.ExecutionProvenance
}

// PreparedEdits allows host-approved changes without resolving live sources.
// Nil Tools keeps the baseline list; a pointer to an empty list denies all.
// The host must enforce its edit policy and any narrower sampling bounds.
type PreparedEdits struct {
	SectionBodies    map[string]string
	ToolDescriptions map[string]string
	Tools            *[]string
	Temperature      *float64
}

// Inspect returns independently decoded metadata and content.
func (p *PreparedAgent) Inspect() (PreparedView, error) {
	snap, err := p.snapshot()
	if err != nil {
		return PreparedView{}, err
	}
	provenance := snap.Provenance
	provenance.PreparedDigest = p.Digest()
	return PreparedView{AgentID: snap.Agent.ID, Scope: snap.Scope, Config: PreparedConfig(snap.Config), Sections: snap.Sections, Tools: snap.Tools, Provenance: provenance}, nil
}

// Derive changes only existing unlocked bodies, tool guidance/subsets and
// temperature in [0,2]. Identity, scope, model, budgets, schema and host
// implementation pins cannot change. It performs no store or provider reads.
func (p *PreparedAgent) Derive(edits PreparedEdits) (*PreparedAgent, error) {
	snap, err := p.snapshot()
	if err != nil {
		return nil, err
	}
	for sectionID, body := range edits.SectionBodies {
		matched := 0
		for i := range snap.Sections {
			section := &snap.Sections[i]
			if section.ID != sectionID {
				continue
			}
			if section.Locked {
				return nil, fmt.Errorf("cortex: section %q is locked: %w", sectionID, cortex.ErrInvalidPrepared)
			}
			section.Body = body
			matched++
		}
		if matched != 1 {
			return nil, fmt.Errorf("cortex: section %q is absent or ambiguous: %w", sectionID, cortex.ErrInvalidPrepared)
		}
	}
	for name, description := range edits.ToolDescriptions {
		matched := 0
		for i := range snap.Tools {
			if snap.Tools[i].Name == name {
				snap.Tools[i].Description = description
				matched++
			}
		}
		if matched != 1 {
			return nil, fmt.Errorf("cortex: tool %q is absent or ambiguous: %w", name, cortex.ErrInvalidPrepared)
		}
	}
	if edits.Tools != nil {
		seen := make(map[string]bool, len(*edits.Tools))
		for _, name := range *edits.Tools {
			if seen[name] || len(filterByName(snap.Tools, []string{name})) != 1 {
				return nil, fmt.Errorf("cortex: invalid tool subset %q: %w", name, cortex.ErrInvalidPrepared)
			}
			seen[name] = true
		}
		snap.Tools = filterByName(snap.Tools, *edits.Tools)
		snap.Config.Tools = append([]string(nil), (*edits.Tools)...)
		snap.Config.ToolsRestricted = true
		snap.Config.AllToolsRestricted = true
	}
	if edits.Temperature != nil {
		value := *edits.Temperature
		if math.IsNaN(value) || value < 0 || value > 2 {
			return nil, fmt.Errorf("cortex: temperature must be finite and between zero and two: %w", cortex.ErrInvalidPrepared)
		}
		snap.Config.Temperature = &value
	}
	snap.Provenance.DerivedFrom = p.Digest()
	return freezePrepared(snap)
}
