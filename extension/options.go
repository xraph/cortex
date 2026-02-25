package extension

import (
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/store"
)

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
	return func(e *Extension) { e.config = cfg }
}

// WithDisableRoutes disables HTTP route registration.
func WithDisableRoutes() ExtOption {
	return func(e *Extension) { e.config.DisableRoutes = true }
}

// WithDisableMigrate disables auto-migration on start.
func WithDisableMigrate() ExtOption {
	return func(e *Extension) { e.config.DisableMigrate = true }
}

// WithBasePath sets the URL prefix for all cortex routes.
func WithBasePath(path string) ExtOption {
	return func(e *Extension) { e.config.BasePath = path }
}

// WithRequireConfig requires config to be present in YAML files.
// If true and no config is found, Register returns an error.
func WithRequireConfig(require bool) ExtOption {
	return func(e *Extension) { e.config.RequireConfig = require }
}

// WithGroveDatabase sets the name of the grove.DB to resolve from the DI container.
// The extension will auto-construct the appropriate store backend (postgres/sqlite/mongo)
// based on the grove driver type. Pass an empty string to use the default (unnamed) grove.DB.
func WithGroveDatabase(name string) ExtOption {
	return func(e *Extension) {
		e.config.GroveDatabase = name
		e.useGrove = true
	}
}
