// Package engine provides the central Cortex agent orchestration coordinator.
package engine

import (
	"log/slog"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/plugin"
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
func WithLogger(l *slog.Logger) Option {
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
