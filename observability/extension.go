// Package observability used to hold Cortex's metrics plugin.
//
// Deprecated: the engine records its own metrics now, labelled by state,
// stop reason, tool kind, publication source and the rest. Pass
// engine.WithMetrics, or let the Forge extension pass app.Metrics() for
// you, and drop this plugin. It implements no hooks and records nothing.
// The dotted counters it used to declare (cortex.agent.run.started and the
// other fourteen) are gone, and so are the struct fields that exposed them.
package observability

import (
	gu "github.com/xraph/go-utils/metrics"

	"github.com/xraph/cortex/plugin"
)

var _ plugin.Extension = (*MetricsExtension)(nil)

// MetricsExtension is a plugin that implements no hooks.
//
// Deprecated: use engine.WithMetrics. The Forge extension wires
// app.Metrics() into the engine without it.
type MetricsExtension struct{}

// NewMetricsExtension returns a plugin that records nothing.
//
// Deprecated: use engine.WithMetrics.
func NewMetricsExtension() *MetricsExtension { return &MetricsExtension{} }

// NewMetricsExtensionWithFactory returns a plugin that records nothing. The
// factory is ignored.
//
// Deprecated: use engine.WithMetrics with the same metrics.
func NewMetricsExtensionWithFactory(_ gu.MetricFactory) *MetricsExtension {
	return &MetricsExtension{}
}

// Name implements plugin.Extension.
func (m *MetricsExtension) Name() string { return "observability-metrics" }
