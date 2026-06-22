package fabriqbrain

import (
	"github.com/xraph/vessel"

	"github.com/xraph/cortex/engine"

	"github.com/xraph/fabriq"
	"github.com/xraph/fabriq/core/agent"
)

// brainToolkit is the union of toolkit capabilities the engine options need.
// *agent.Toolkit satisfies it.
type brainToolkit interface {
	recaller
	toolLister
	rememberer
}

// buildToolkit constructs a fabriq agent toolkit from the facade and config.
// NOTE: *fabriq.Fabriq exposes Registry() but not CAS (CAS lives on
// forgeext.Extension, which is not what we inject). The digest/resolve tools run
// with a nil CAS — fabriq's toolkit supports that (no CAS-backed summary text).
// Wiring CAS would require injecting the Forge extension instead; out of scope.
func buildToolkit(f *fabriq.Fabriq, c config) (*agent.Toolkit, error) {
	return agent.NewToolkit(f, f.Registry(), c.embedder, agent.Config{Write: c.writePolicy})
}

// engineOptions bundles the knowledge provider, rich tools, and learning-loop
// plugin for a toolkit. EngineOptions wraps this after resolving the facade.
func engineOptions(tk brainToolkit, cfg config, opts []Option) []engine.Option {
	out := []engine.Option{engine.WithKnowledge(NewProvider(tk, opts...))}
	out = append(out, toolOptions(tk, cfg)...)
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
	tk, err := buildToolkit(f, cfg)
	if err != nil {
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
	tk, err := buildToolkit(f, cfg)
	if err != nil {
		return nil
	}
	return engineOptions(tk, cfg, opts)
}
