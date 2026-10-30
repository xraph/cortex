package extension

import (
	"github.com/xraph/forge"

	"github.com/xraph/cortex/engine"
	weaveknowledge "github.com/xraph/cortex/knowledge/weave"
	nexusllm "github.com/xraph/cortex/llm/nexus"
	"github.com/xraph/cortex/plugin"
	shieldsafety "github.com/xraph/cortex/safety/shield"
	"github.com/xraph/cortex/store"
)

// ExtOption configures the Cortex Forge extension.
type ExtOption func(*Extension)

// WithStore sets the composite store. It wins over any grove database in
// the container, and the extension then skips discovering a default one.
func WithStore(s store.Store) ExtOption {
	return func(e *Extension) {
		e.hostStore = true
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

// WithoutAutoAdapters stops the extension from discovering nexus, shield,
// weave and sentinel in the DI container. Without it, each one found is
// wired into the engine: nexus as the model client, shield as the safety
// scanner and the publication gate, weave as the knowledge provider
// and sentinel as an extension. Use it when the host wires those itself,
// or wants none of them.
func WithoutAutoAdapters() ExtOption {
	return func(e *Extension) { e.noAutoAdapters = true }
}

// WithNexusOptions configures the nexus adapter the extension wires in when
// it finds a nexus gateway in the container: nexusllm.WithTenant to map a
// scope to its nexus tenant, nexusllm.WithProvider, WithMetadata and
// WithSamplingUnsupported. The adapter keeps its place in the option order,
// so an engine.WithLLM passed with WithEngineOption still replaces it, and
// WithoutAutoAdapters still leaves it out.
func WithNexusOptions(opts ...nexusllm.Option) ExtOption {
	return func(e *Extension) { e.nexusOpts = append(e.nexusOpts, opts...) }
}

// WithWeaveOptions configures the weave adapter the extension wires in when
// it finds a weave engine in the container: weaveknowledge.WithTenant to map
// a scope to its weave tenant, and weaveknowledge.WithShared to add
// read-only collections every run may search. The adapter keeps its place in
// the option order, so an engine.WithKnowledge passed with WithEngineOption
// still replaces it, and WithoutAutoAdapters still leaves it out.
func WithWeaveOptions(opts ...weaveknowledge.Option) ExtOption {
	return func(e *Extension) { e.weaveOpts = append(e.weaveOpts, opts...) }
}

// WithShieldOptions configures the shield adapter the extension wires in
// when it finds a shield engine in the container: shieldsafety.WithTenant
// to map a scope to its shield tenant, shieldsafety.WithApp to name the
// shield app, and shieldsafety.WithUntrustedSources. The adapter keeps its
// place in the option order, so an engine.WithSafety or
// engine.WithPublicationGate passed with WithEngineOption still replaces
// the matching half of it, and WithoutAutoAdapters still leaves it out.
func WithShieldOptions(opts ...shieldsafety.Option) ExtOption {
	return func(e *Extension) { e.shieldOpts = append(e.shieldOpts, opts...) }
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

// WithRoutes mounts an HTTP surface on the extension.
//
// The REST handlers live in github.com/xraph/cortex/api, a separate module,
// so nothing here forces a host to compile them in. Pass this when you want
// them:
//
//	extension.New(
//		extension.WithRoutes(func(eng *engine.Engine, r forge.Router) extension.RouteSet {
//			return api.New(eng, r)
//		}),
//	)
//
// Leave it out and the extension registers no routes, Routes returns nil and
// Handler answers 404. The engine, the dashboard and the DI wiring are
// unaffected either way.
func WithRoutes(build func(eng *engine.Engine, router forge.Router) RouteSet) ExtOption {
	return func(e *Extension) {
		e.buildRoutes = build
	}
}
