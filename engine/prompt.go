package engine

import (
	"context"
	"strings"

	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/knowledge"
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

// BuildSystemPrompt assembles the full system prompt from agent config,
// persona identity, skill fragments, and trait injections.
// This is the engine-level equivalent of dashboard/data.go:computeSystemPrompt.
func (e *Engine) BuildSystemPrompt(ctx context.Context, ag *agent.Config, overrides *RunOverrides) string {
	var parts []string

	// Determine effective system prompt.
	systemPrompt := ag.SystemPrompt
	if overrides != nil && overrides.SystemPrompt != "" {
		systemPrompt = overrides.SystemPrompt
	}
	if systemPrompt != "" {
		parts = append(parts, systemPrompt)
	}

	// Determine effective persona ref.
	personaRef := ag.PersonaRef
	if overrides != nil && overrides.PersonaRef != "" {
		personaRef = overrides.PersonaRef
	}

	// Resolve persona identity.
	if personaRef != "" && e.store != nil {
		p, err := e.store.GetPersonaByName(ctx, ag.AppID, personaRef)
		if err == nil && p.Identity != "" {
			parts = append(parts, "\n## Identity\n"+p.Identity)
		}
	}

	// Determine effective skills.
	skillNames := ag.InlineSkills
	if overrides != nil && len(overrides.InlineSkills) > 0 {
		skillNames = overrides.InlineSkills
	}

	// Inject skill prompt fragments.
	if e.store != nil {
		for _, sName := range skillNames {
			sName = strings.TrimSpace(sName)
			if sName == "" {
				continue
			}
			sk, err := e.store.GetSkillByName(ctx, ag.AppID, sName)
			if err == nil && sk.SystemPromptFragment != "" {
				parts = append(parts, "\n## Skill: "+sk.Name+"\n"+sk.SystemPromptFragment)
			}
		}
	}

	// Inject knowledge from skill KnowledgeRef entries.
	if e.knowledge != nil && e.store != nil {
		for _, sName := range skillNames {
			sName = strings.TrimSpace(sName)
			if sName == "" {
				continue
			}
			sk, err := e.store.GetSkillByName(ctx, ag.AppID, sName)
			if err != nil || len(sk.Knowledge) == 0 {
				continue
			}
			for _, kref := range sk.Knowledge {
				if kref.Source == "" {
					continue
				}
				chunks, kErr := e.knowledge.Retrieve(ctx, kref.Source, &knowledge.RetrieveParams{
					TopK: 5,
				})
				if kErr != nil || len(chunks) == 0 {
					continue
				}
				var kb strings.Builder
				kb.WriteString("\n## Knowledge: " + kref.Source + "\n")
				for _, c := range chunks {
					kb.WriteString("- " + c.Content + "\n")
				}
				parts = append(parts, kb.String())
			}
		}
	}

	// Determine effective traits.
	traitNames := ag.InlineTraits
	if overrides != nil && len(overrides.InlineTraits) > 0 {
		traitNames = overrides.InlineTraits
	}

	// Inject trait prompt injections.
	if e.store != nil {
		for _, tName := range traitNames {
			tName = strings.TrimSpace(tName)
			if tName == "" {
				continue
			}
			t, err := e.store.GetTraitByName(ctx, ag.AppID, tName)
			if err == nil {
				for _, inf := range t.Influences {
					if inf.Target == "prompt_injection" {
						if v, ok := inf.Value.(string); ok && v != "" {
							parts = append(parts, "\n## Trait: "+t.Name+"\n"+v)
						}
					}
				}
			}
		}
	}

	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "\n")
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
