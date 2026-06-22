package fabriqbrain

import (
	"github.com/xraph/vessel"

	"github.com/xraph/cortex/engine"

	"github.com/xraph/fabriq"
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

// fabricFacade is the narrow slice of *fabriq.Fabriq buildToolkit needs: the
// query surface (passed to NewToolkit) plus its entity registry. Depending on
// the interface (not the concrete facade) keeps buildToolkit unit-testable.
type fabricFacade interface {
	query.Fabric
	Registry() *registry.Registry
}

// buildToolkit constructs a fabriq agent toolkit from the facade and config.
// VectorDims is threaded only when set (>0) so a 0 leaves fabriq's own 768
// default in force.
// NOTE: *fabriq.Fabriq exposes Registry() but not CAS (CAS lives on
// forgeext.Extension, which is not what we inject). The digest/resolve tools run
// with a nil CAS — fabriq's toolkit supports that (no CAS-backed summary text).
// Wiring CAS would require injecting the Forge extension instead; out of scope.
func buildToolkit(f fabricFacade, c config) (*agent.Toolkit, error) {
	acfg := agent.Config{Write: c.writePolicy}
	if c.vectorDims > 0 {
		acfg.VectorDims = c.vectorDims
	}
	return agent.NewToolkit(f, f.Registry(), c.embedder, acfg)
}

// resolveToolkit builds the toolkit and, on failure, logs via the configured
// logger before returning nil. This surfaces misconfiguration — most notably an
// embedder/vector-dims mismatch — instead of silently degrading the brain to a
// no-op with no diagnostic.
func resolveToolkit(f fabricFacade, c config) *agent.Toolkit {
	tk, err := buildToolkit(f, c)
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

// EngineOption wires ONLY the knowledge provider (parity with
// weave.EngineOption). Returns a no-op option when no fabriq facade is in the
// container.
func EngineOption(c vessel.Vessel, opts ...Option) engine.Option {
	f, err := vessel.Inject[*fabriq.Fabriq](c)
	if err != nil {
		return func(_ *engine.Engine) error { return nil }
	}
	cfg := applyOptions(opts)
	tk := resolveToolkit(f, cfg)
	if tk == nil {
		return func(_ *engine.Engine) error { return nil }
	}
	return engine.WithKnowledge(NewProvider(tk, opts...))
}

// EngineOptions wires the FULL brain: knowledge provider + rich tools +
// learning-loop plugin. Returns nil (safe to spread) when no fabriq facade is
// present.
func EngineOptions(c vessel.Vessel, opts ...Option) []engine.Option {
	f, err := vessel.Inject[*fabriq.Fabriq](c)
	if err != nil {
		return nil
	}
	cfg := applyOptions(opts)
	tk := resolveToolkit(f, cfg)
	if tk == nil {
		return nil
	}
	return engineOptions(tk, cfg, opts)
}
