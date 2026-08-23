package fabriqbrain

import (
	"github.com/xraph/vessel"

	"github.com/xraph/cortex/engine"

	"github.com/xraph/fabriq/core/agent"
	"github.com/xraph/fabriq/core/query"
	"github.com/xraph/fabriq/core/registry"
	log "github.com/xraph/go-utils/log"
)

// brainToolkit is the union of toolkit capabilities the engine options need.
// *agent.Toolkit satisfies it.
type brainToolkit interface {
	recaller
	toolLister
	rememberer
}

// buildToolkit constructs a fabriq agent toolkit from the query port, the
// entity registry, and config. VectorDims is threaded only when set (>0) so a 0
// leaves fabriq's own 768 default in force.
//
// The registry arrives as its own argument rather than off the facade.
// query.Fabric has no Registry() method by design: entity specs are declared
// client-side and an engine reached over the wire cannot serve them. Taking it
// separately is what lets this wiring accept a remote fabric unchanged.
//
// NOTE: neither the facade nor the registry carries CAS (it lives on
// forgeext.Extension, which is not what we inject). The digest/resolve tools run
// with a nil CAS — fabriq's toolkit supports that (no CAS-backed summary text).
// Wiring CAS would require injecting the Forge extension instead; out of scope.
func buildToolkit(f query.Fabric, reg *registry.Registry, c config) (*agent.Toolkit, error) {
	acfg := agent.Config{Write: c.writePolicy}
	if c.vectorDims > 0 {
		acfg.VectorDims = c.vectorDims
	}
	return agent.NewToolkit(f, reg, c.embedder, acfg)
}

// resolveToolkit builds the toolkit and, on failure, logs via the configured
// logger before returning nil. This surfaces misconfiguration — most notably an
// embedder/vector-dims mismatch — instead of silently degrading the brain to a
// no-op with no diagnostic.
func resolveToolkit(f query.Fabric, reg *registry.Registry, c config) *agent.Toolkit {
	tk, err := buildToolkit(f, reg, c)
	if err != nil {
		c.logger.Error("fabriq-brain: toolkit build failed; brain not wired", log.String("error", err.Error()))
		return nil
	}
	return tk
}

// engineOptions bundles the knowledge provider, rich tools, and learning-loop
// plugin for a toolkit. EngineOptions wraps this after resolving the facade.
func engineOptions(tk brainToolkit, cfg config, opts []Option) []engine.Option {
	toolOpts := toolOptions(tk, cfg)
	out := make([]engine.Option, 0, len(toolOpts)+2)
	out = append(out, engine.WithKnowledge(NewProvider(tk, opts...)))
	out = append(out, toolOpts...)
	out = append(out, engine.WithExtension(NewPlugin(tk, opts...)))
	return out
}

// injectFabric resolves the fabriq query port and its entity registry from the
// container. It keys on query.Fabric rather than the concrete *fabriq.Fabriq so
// cortex never imports fabriq's composition root, and so a deployment fronting
// a remote engine resolves here identically.
//
// The two failures are not the same and are not treated the same. No facade in
// the container means the app does not use fabriq, which is ordinary and stays
// quiet. A facade WITHOUT its registry means fabriq is present but wired wrong,
// so that one is logged before it degrades the brain to a no-op.
func injectFabric(c vessel.Vessel, logger log.Logger) (query.Fabric, *registry.Registry, error) {
	f, err := vessel.Inject[query.Fabric](c)
	if err != nil {
		return nil, nil, err
	}
	reg, err := vessel.InjectNamed[*registry.Registry](c, registry.ServiceName)
	if err != nil {
		logger.Error(
			"fabriq-brain: fabric resolved but entity registry "+registry.ServiceName+" did not; brain not wired",
			log.String("error", err.Error()),
		)
		return nil, nil, err
	}
	return f, reg, nil
}

// EngineOption wires ONLY the knowledge provider (parity with
// weave.EngineOption). Returns a no-op option when no fabriq facade is in the
// container.
func EngineOption(c vessel.Vessel, opts ...Option) engine.Option {
	cfg := applyOptions(opts)
	f, reg, err := injectFabric(c, cfg.logger)
	if err != nil {
		return func(_ *engine.Engine) error { return nil }
	}
	tk := resolveToolkit(f, reg, cfg)
	if tk == nil {
		return func(_ *engine.Engine) error { return nil }
	}
	return engine.WithKnowledge(NewProvider(tk, opts...))
}

// EngineOptions wires the FULL brain: knowledge provider + rich tools +
// learning-loop plugin. Returns nil (safe to spread) when no fabriq facade is
// present.
func EngineOptions(c vessel.Vessel, opts ...Option) []engine.Option {
	cfg := applyOptions(opts)
	f, reg, err := injectFabric(c, cfg.logger)
	if err != nil {
		return nil
	}
	tk := resolveToolkit(f, reg, cfg)
	if tk == nil {
		return nil
	}
	return engineOptions(tk, cfg, opts)
}
