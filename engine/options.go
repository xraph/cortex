// Package engine provides the central Cortex agent orchestration coordinator.
package engine

import (
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
