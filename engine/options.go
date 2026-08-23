// Package engine provides the central Cortex agent orchestration coordinator.
package engine

import (
	"context"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/store"
)

// Option configures the Engine.
type Option func(*Engine) error

// WithStore sets the composite store.
func WithStore(s store.Store) Option {
	return func(e *Engine) error {
		e.store = s
		return nil
	}
}

// WithExtension registers an extension with the engine.
func WithExtension(ext plugin.Extension) Option {
	return func(e *Engine) error {
		e.pendingExts = append(e.pendingExts, ext)
		return nil
	}
}

// WithLogger sets the structured logger.
func WithLogger(l log.Logger) Option {
	return func(e *Engine) error {
		e.logger = l
		return nil
	}
}

// WithConfig sets the engine configuration.
func WithConfig(cfg cortex.Config) Option {
	return func(e *Engine) error {
		e.config = cfg
		return nil
	}
}

// WithLLM sets the LLM client for real model execution.
// When set, RunAgent and StreamAgent use this client instead of mock/echo mode.
func WithLLM(client llm.Client) Option {
	return func(e *Engine) error {
		e.llm = client
		return nil
	}
}

// WithSafety sets the safety scanner for content scanning.
// When set, RunAgent and StreamAgent scan input before LLM calls
// and scan output after LLM responses.
func WithSafety(scanner safety.Scanner) Option {
	return func(e *Engine) error {
		e.safety = scanner
		return nil
	}
}

// WithKnowledge sets the knowledge provider for RAG-based knowledge retrieval.
// When set, skills with KnowledgeRef entries can inject relevant context
// into agent system prompts and agents gain access to a knowledge_search tool.
func WithKnowledge(provider knowledge.Provider) Option {
	return func(e *Engine) error {
		e.knowledge = provider
		return nil
	}
}

// ToolHandler executes a registered tool. The Invocation carries the
// scope, the host's principal and the call itself, so a handler never
// has to reach into the context for them.
type ToolHandler func(ctx context.Context, inv cortex.Invocation) (string, error)

// WithTool registers an externally-provided executable tool. The def is
// advertised to the LLM (resolveTools); the handler runs when the model calls
// it (executeTool). Registering tools with the same name appends both; the
// first match wins at dispatch.
func WithTool(def llm.Tool, h ToolHandler) Option {
	return func(e *Engine) error {
		e.tools = append(e.tools, registeredTool{def: def, handler: h})
		return nil
	}
}

// WithToolAuthorizer installs the host's authorization decisions. A nil
// authorizer, or none at all, allows every tool and every call.
func WithToolAuthorizer(a cortex.ToolAuthorizer) Option {
	return func(e *Engine) error {
		e.authorizer = a
		return nil
	}
}

// WithExternalTool registers a tool the engine advertises but never
// executes. When the model calls one, the run suspends and waits for the
// caller to run it and hand the result back.
//
// The definition is advertised exactly like a WithTool registration
// (resolveTools), and the authorizer gates a call to it exactly like any
// other (executeTool). The only difference is what happens at dispatch:
// there is no handler to run, so the call becomes pending and the loop
// stops.
func WithExternalTool(def llm.Tool) Option {
	return func(e *Engine) error {
		e.externalTools = append(e.externalTools, def)
		return nil
	}
}
