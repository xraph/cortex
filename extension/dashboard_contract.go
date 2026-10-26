package extension

import (
	"fmt"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	cortexcontract "github.com/xraph/cortex/extension/contract"
	"github.com/xraph/cortex/optimization"
)

// WithDashboard supplies the authenticated scope mapping and durable audit sink.
// Engine is supplied by the extension after initialization.
func WithDashboard(deps cortexcontract.Deps) ExtOption {
	return func(e *Extension) { e.dashboardDeps = deps }
}
func (e *Extension) RegisterContractContributor(d *dispatcher.Dispatcher, reg dc.Registry, wreg dc.WardenRegistry) error {
	if e.eng == nil {
		return fmt.Errorf("cortex: dashboard requires an initialized engine")
	}
	deps := e.dashboardDeps
	deps.Engine = e.eng
	if e.optimization != nil {
		deps.Optimization = e.optimization
	}
	return cortexcontract.Register(d, reg, wreg, deps)
}

// WithOptimization supplies the dashboard's operator service. Worker scope
// enumeration and shutdown remain with the host that owns the optimizer.
// This option takes precedence over the service supplied through WithDashboard.
func WithOptimization(value *optimization.Optimizer) ExtOption {
	return func(e *Extension) { e.optimization = value }
}
