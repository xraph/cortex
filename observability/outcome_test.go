package observability

import (
	"context"
	"testing"

	gu "github.com/xraph/go-utils/metrics"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/run"
)

// The deprecated plugin still registers, so a host that keeps it compiles
// and runs, but it subscribes to nothing. The engine's own metrics replace
// it.
func TestDeprecatedPluginImplementsNoHooks(t *testing.T) {
	var ext any = NewMetricsExtensionWithFactory(gu.NewMetricsCollector("cortex-test"))
	if _, ok := ext.(plugin.RunTerminated); ok {
		t.Error("the deprecated plugin still subscribes to RunTerminated")
	}
	if _, ok := ext.(plugin.RunStarted); ok {
		t.Error("the deprecated plugin still subscribes to RunStarted")
	}
	if _, ok := ext.(plugin.ToolCalled); ok {
		t.Error("the deprecated plugin still subscribes to ToolCalled")
	}

	registry := plugin.NewRegistry(nil)
	registry.Register(NewMetricsExtension())
	registry.EmitRunTerminated(context.Background(), id.NewAgentID(), id.NewAgentRunID(),
		run.TerminalSummary{State: run.StateIncomplete, StopReason: run.StopPolicyUnavailable})
}
