// Package fabriqbrain adapts the fabriq agent toolkit to cortex as a
// plug-n-play brain: a knowledge.Provider for recall, rich tools, and a
// learning-loop plugin. The package directory is integrations/fabriq; the
// package name is fabriqbrain to avoid colliding with github.com/xraph/fabriq.
package fabriqbrain

import (
	"context"

	"github.com/xraph/fabriq/core/agent"
	log "github.com/xraph/go-utils/log"
)

// config holds bridge configuration shared by the provider, tools, and plugin.
type config struct {
	embedder     agent.Embedder
	entities     []string
	budget       int
	memoryEntity string
	writePolicy  agent.WritePolicy
	tenant       func(context.Context) context.Context
	render       func(agent.ContextItem) string
	logger       log.Logger
	vectorDims   int
}

// Option configures the bridge.
type Option func(*config)

func defaultConfig() config {
	return config{
		budget:       4096,
		memoryEntity: "agent_memory",
		tenant:       func(ctx context.Context) context.Context { return ctx },
		logger:       log.NewNoopLogger(),
	}
}

func applyOptions(opts []Option) config {
	c := defaultConfig()
	for _, o := range opts {
		o(&c)
	}
	return c
}

// WithEmbedder supplies the embedding model for recall's vector channel. It
// MUST match the embedder fabriq's index/embed worker used (same model + dims);
// otherwise the vector channel mismatches. When nil, recall degrades to
// full-text + graph channels only.
func WithEmbedder(e agent.Embedder) Option { return func(c *config) { c.embedder = e } }

// WithEntities sets the fabriq entity types recall searches and ListCollections
// reports. Required for recall to return results (fabriq's Recall needs at least
// one entity).
func WithEntities(entities ...string) Option {
	return func(c *config) { c.entities = append([]string(nil), entities...) }
}

// WithBudget sets the token budget for each recall (default 4096).
func WithBudget(n int) Option { return func(c *config) { c.budget = n } }

// WithMemoryEntity sets the fabriq entity the learning-loop plugin writes agent
// activity into (default "agent_memory").
func WithMemoryEntity(entity string) Option { return func(c *config) { c.memoryEntity = entity } }

// WithWritePolicy sets the guarded-write allowlist used by the remember tool and
// the learning-loop plugin. Empty = no writes permitted (deny-by-default).
func WithWritePolicy(p agent.WritePolicy) Option { return func(c *config) { c.writePolicy = p } }

// WithTenantMapper overrides how a cortex request context is translated into the
// scope fabriq reads. Default is identity (correct when both share Forge scope).
func WithTenantMapper(fn func(context.Context) context.Context) Option {
	return func(c *config) {
		if fn != nil {
			c.tenant = fn
		}
	}
}

// WithRenderer overrides how a recalled ContextItem becomes chunk text. Default
// renders the row JSON verbatim.
func WithRenderer(fn func(agent.ContextItem) string) Option {
	return func(c *config) { c.render = fn }
}

// WithLogger sets the logger used to report swallowed memory-write failures in
// the learning-loop plugin and toolkit-build failures during wiring. Default is
// a no-op logger.
func WithLogger(l log.Logger) Option {
	return func(c *config) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithVectorDims sets the embedding dimensionality the toolkit expects, which
// MUST match the configured embedder's Dims (e.g. 1536 for OpenAI
// text-embedding-3-small). Default 0 leaves it unset so fabriq applies its own
// 768 default — correct for 768-dim embedders. Setting this wrong (or leaving
// the default with a non-768 embedder) makes NewToolkit fail with a dims
// mismatch, which the wiring path now surfaces via the configured logger.
func WithVectorDims(n int) Option { return func(c *config) { c.vectorDims = n } }
