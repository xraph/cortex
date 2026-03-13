package shared

import (
	"context"
	"time"
)

// --- Tool Discovery Types ---

// DiscoveredTool aggregates tool information from agents, skills, and run history.
type DiscoveredTool struct {
	Name          string
	AgentRefs     []string       // agent names that reference this tool
	SkillBindings []SkillToolRef // skills that bind this tool
	TotalCalls    int            // total invocations from run history
	TotalErrors   int            // tool calls with non-empty Error
	LastUsed      *time.Time
}

// SkillToolRef summarises a tool's binding within a skill.
type SkillToolRef struct {
	SkillName  string
	Mastery    string
	Guidance   string
	PreferWhen string
}

// Source returns a human-readable source label: "agents", "skills", or "both".
func (d *DiscoveredTool) Source() string {
	hasAgent := len(d.AgentRefs) > 0
	hasSkill := len(d.SkillBindings) > 0
	switch {
	case hasAgent && hasSkill:
		return "both"
	case hasAgent:
		return "agents"
	case hasSkill:
		return "skills"
	default:
		return "runs"
	}
}

// ErrorRate returns the percentage of tool calls that errored.
func (d *DiscoveredTool) ErrorRate() float64 {
	if d.TotalCalls == 0 {
		return 0
	}
	return float64(d.TotalErrors) / float64(d.TotalCalls) * 100
}

// --- Model Discovery Types ---

// ModelSource provides an abstraction over LLM model discovery.
// When nexus is available, it adapts the nexus gateway; otherwise
// the dashboard works without it.
type ModelSource interface {
	ListModels(ctx context.Context) ([]ModelInfo, error)
	ListProviders(ctx context.Context) ([]ProviderInfo, error)
}

// ModelInfo is a dashboard-level representation of an LLM model.
type ModelInfo struct {
	ID            string
	Provider      string
	Name          string
	ContextWindow int
	MaxOutput     int
	InputPricing  float64 // USD per million tokens
	OutputPricing float64 // USD per million tokens
	Capabilities  ModelCapabilities
}

// ModelCapabilities describes what an LLM model supports.
type ModelCapabilities struct {
	Chat       bool
	Streaming  bool
	Embeddings bool
	Images     bool
	Vision     bool
	Tools      bool
	JSON       bool
	Audio      bool
	Thinking   bool
	Batch      bool
}

// CapabilityNames returns the names of enabled capabilities.
func (c ModelCapabilities) CapabilityNames() []string {
	var caps []string
	if c.Chat {
		caps = append(caps, "chat")
	}
	if c.Streaming {
		caps = append(caps, "streaming")
	}
	if c.Embeddings {
		caps = append(caps, "embeddings")
	}
	if c.Images {
		caps = append(caps, "images")
	}
	if c.Vision {
		caps = append(caps, "vision")
	}
	if c.Tools {
		caps = append(caps, "tools")
	}
	if c.JSON {
		caps = append(caps, "json")
	}
	if c.Audio {
		caps = append(caps, "audio")
	}
	if c.Thinking {
		caps = append(caps, "thinking")
	}
	if c.Batch {
		caps = append(caps, "batch")
	}
	return caps
}

// ProviderInfo summarises an LLM provider for the dashboard.
type ProviderInfo struct {
	Name       string
	ModelCount int
	Healthy    bool
}
