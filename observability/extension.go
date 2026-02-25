// Package observability provides a metrics extension for Cortex that records
// lifecycle event counts via go-utils MetricFactory.
package observability

import (
	"context"
	"time"

	gu "github.com/xraph/go-utils/metrics"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/plugin"
)

// Compile-time interface checks.
var (
	_ plugin.Extension             = (*MetricsExtension)(nil)
	_ plugin.RunStarted            = (*MetricsExtension)(nil)
	_ plugin.RunCompleted          = (*MetricsExtension)(nil)
	_ plugin.RunFailed             = (*MetricsExtension)(nil)
	_ plugin.ToolCalled            = (*MetricsExtension)(nil)
	_ plugin.ToolCompleted         = (*MetricsExtension)(nil)
	_ plugin.ToolFailed            = (*MetricsExtension)(nil)
	_ plugin.PersonaResolved       = (*MetricsExtension)(nil)
	_ plugin.BehaviorTriggered     = (*MetricsExtension)(nil)
	_ plugin.CognitivePhaseChanged = (*MetricsExtension)(nil)
	_ plugin.CheckpointCreated     = (*MetricsExtension)(nil)
	_ plugin.CheckpointResolved    = (*MetricsExtension)(nil)
)

// MetricsExtension records lifecycle metrics via go-utils MetricFactory.
type MetricsExtension struct {
	RunStartedCount            gu.Counter
	RunCompletedCount          gu.Counter
	RunFailedCount             gu.Counter
	ToolCalledCount            gu.Counter
	ToolCompletedCount         gu.Counter
	ToolFailedCount            gu.Counter
	PersonaResolvedCount       gu.Counter
	BehaviorTriggeredCount     gu.Counter
	CognitivePhaseChangedCount gu.Counter
	CheckpointCreatedCount     gu.Counter
	CheckpointResolvedCount    gu.Counter
}

// NewMetricsExtension creates a MetricsExtension with a default metrics collector.
func NewMetricsExtension() *MetricsExtension {
	return NewMetricsExtensionWithFactory(gu.NewMetricsCollector("cortex/observability"))
}

// NewMetricsExtensionWithFactory creates a MetricsExtension with the provided MetricFactory.
func NewMetricsExtensionWithFactory(factory gu.MetricFactory) *MetricsExtension {
	return &MetricsExtension{
		RunStartedCount:            factory.Counter("cortex.agent.run.started"),
		RunCompletedCount:          factory.Counter("cortex.agent.run.completed"),
		RunFailedCount:             factory.Counter("cortex.agent.run.failed"),
		ToolCalledCount:            factory.Counter("cortex.tool.called"),
		ToolCompletedCount:         factory.Counter("cortex.tool.completed"),
		ToolFailedCount:            factory.Counter("cortex.tool.failed"),
		PersonaResolvedCount:       factory.Counter("cortex.persona.resolved"),
		BehaviorTriggeredCount:     factory.Counter("cortex.behavior.triggered"),
		CognitivePhaseChangedCount: factory.Counter("cortex.cognitive.phase_changed"),
		CheckpointCreatedCount:     factory.Counter("cortex.checkpoint.created"),
		CheckpointResolvedCount:    factory.Counter("cortex.checkpoint.resolved"),
	}
}

// Name implements plugin.Extension.
func (m *MetricsExtension) Name() string { return "observability-metrics" }

func (m *MetricsExtension) OnRunStarted(_ context.Context, _ id.AgentID, _ id.AgentRunID, _ string) error {
	m.RunStartedCount.Inc()
	return nil
}

func (m *MetricsExtension) OnRunCompleted(_ context.Context, _ id.AgentID, _ id.AgentRunID, _ string, _ time.Duration) error {
	m.RunCompletedCount.Inc()
	return nil
}

func (m *MetricsExtension) OnRunFailed(_ context.Context, _ id.AgentID, _ id.AgentRunID, _ error) error {
	m.RunFailedCount.Inc()
	return nil
}

func (m *MetricsExtension) OnToolCalled(_ context.Context, _ id.AgentRunID, _ string, _ any) error {
	m.ToolCalledCount.Inc()
	return nil
}

func (m *MetricsExtension) OnToolCompleted(_ context.Context, _ id.AgentRunID, _, _ string, _ time.Duration) error {
	m.ToolCompletedCount.Inc()
	return nil
}

func (m *MetricsExtension) OnToolFailed(_ context.Context, _ id.AgentRunID, _ string, _ error) error {
	m.ToolFailedCount.Inc()
	return nil
}

func (m *MetricsExtension) OnPersonaResolved(_ context.Context, _ id.AgentID, _ string) error {
	m.PersonaResolvedCount.Inc()
	return nil
}

func (m *MetricsExtension) OnBehaviorTriggered(_ context.Context, _ id.AgentRunID, _ string) error {
	m.BehaviorTriggeredCount.Inc()
	return nil
}

func (m *MetricsExtension) OnCognitivePhaseChanged(_ context.Context, _ id.AgentRunID, _, _ string) error {
	m.CognitivePhaseChangedCount.Inc()
	return nil
}

func (m *MetricsExtension) OnCheckpointCreated(_ context.Context, _ id.CheckpointID, _ id.AgentRunID, _ string) error {
	m.CheckpointCreatedCount.Inc()
	return nil
}

func (m *MetricsExtension) OnCheckpointResolved(_ context.Context, _ id.CheckpointID, _ string) error {
	m.CheckpointResolvedCount.Inc()
	return nil
}
