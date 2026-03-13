package sentinel

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/plugin"

	sentinelengine "github.com/xraph/sentinel/engine"
)

// Compile-time interface checks.
var (
	_ plugin.Extension    = (*Plugin)(nil)
	_ plugin.RunCompleted = (*Plugin)(nil)
	_ plugin.Shutdown     = (*Plugin)(nil)
)

// Plugin integrates Sentinel evaluation capabilities into Cortex.
// It implements Cortex's plugin.Extension interface and lifecycle hooks
// to automatically evaluate agent runs when configured.
type Plugin struct {
	eng  *sentinelengine.Engine
	opts pluginOptions
}

// New creates a new Sentinel Cortex plugin.
func New(eng *sentinelengine.Engine, opts ...Option) *Plugin {
	p := &Plugin{eng: eng}
	for _, o := range opts {
		o(&p.opts)
	}
	return p
}

// Name returns the plugin name.
func (p *Plugin) Name() string { return "sentinel" }

// Engine returns the underlying Sentinel engine.
func (p *Plugin) Engine() *sentinelengine.Engine { return p.eng }

// OnRunCompleted is called after every Cortex agent run.
// When auto-eval is enabled and a matching evaluation suite exists,
// it triggers a Sentinel evaluation asynchronously.
func (p *Plugin) OnRunCompleted(_ context.Context, _ id.AgentID, _ id.AgentRunID, _ string, _ time.Duration) error {
	if !p.opts.autoEval {
		return nil
	}
	// TODO: look up eval suites tagged for the agent and trigger evaluation asynchronously.
	return nil
}

// OnShutdown is called during graceful shutdown.
func (p *Plugin) OnShutdown(_ context.Context) error {
	return nil
}
