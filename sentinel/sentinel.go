package sentinel

import (
	"github.com/xraph/vessel"

	"github.com/xraph/cortex/engine"

	sentinelengine "github.com/xraph/sentinel/engine"
)

// EngineOption returns a cortex engine.Option that auto-discovers a Sentinel
// engine from the DI container and registers the Sentinel plugin with Cortex.
// If Sentinel is not available, this is a no-op.
func EngineOption(c vessel.Vessel, opts ...Option) engine.Option {
	return func(e *engine.Engine) error {
		eng, err := vessel.Inject[*sentinelengine.Engine](c)
		if err != nil {
			return nil // Sentinel not available, skip silently.
		}
		return engine.WithExtension(New(eng, opts...))(e)
	}
}
