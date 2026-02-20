package extension

import (
	"log/slog"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/store"
)

// Config configures the Cortex Forge extension.
type Config struct {
	// DisableRoutes prevents HTTP route registration.
	DisableRoutes bool
	// DisableMigrate prevents auto-migration on start.
	DisableMigrate bool
	// BasePath is the URL prefix for all cortex routes.
	BasePath string
}

// ExtOption configures the Cortex Forge extension.
type ExtOption func(*Extension)

// WithStore sets the composite store.
func WithStore(s store.Store) ExtOption {
	return func(e *Extension) {
		e.engineOpts = append(e.engineOpts, engine.WithStore(s))
	}
}

// WithExtension registers a Cortex extension (lifecycle hooks).
func WithExtension(x plugin.Extension) ExtOption {
	return func(e *Extension) {
		e.engineOpts = append(e.engineOpts, engine.WithExtension(x))
	}
}

// WithEngineOption passes an engine option directly.
func WithEngineOption(opt engine.Option) ExtOption {
	return func(e *Extension) {
		e.engineOpts = append(e.engineOpts, opt)
	}
}

// WithConfig sets the extension configuration.
func WithConfig(cfg Config) ExtOption {
	return func(e *Extension) {
		e.config = cfg
	}
}

// WithDisableRoutes disables HTTP route registration.
func WithDisableRoutes() ExtOption {
	return func(e *Extension) {
		e.config.DisableRoutes = true
	}
}

// WithDisableMigrate disables auto-migration on start.
func WithDisableMigrate() ExtOption {
	return func(e *Extension) {
		e.config.DisableMigrate = true
	}
}

// WithBasePath sets the URL prefix for all cortex routes.
func WithBasePath(path string) ExtOption {
	return func(e *Extension) {
		e.config.BasePath = path
	}
}

// WithLogger sets the structured logger.
func WithLogger(l *slog.Logger) ExtOption {
	return func(e *Extension) {
		e.logger = l
	}
}
