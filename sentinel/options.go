// Package sentinel connects the Sentinel engine to Cortex lifecycle hooks.
// Automatic run evaluation is not implemented.
package sentinel

// pluginOptions configures the Sentinel Cortex plugin.
type pluginOptions struct {
	autoEval bool // Automatically evaluate after each Cortex agent run.
}

// Option configures the Plugin.
type Option func(*pluginOptions)

// WithAutoEval enables or disables automatic evaluation after each
// Cortex agent run completes.
func WithAutoEval(enabled bool) Option {
	return func(o *pluginOptions) {
		o.autoEval = enabled
	}
}
