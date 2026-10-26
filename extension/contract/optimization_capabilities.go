package contract

import (
	"context"

	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"

	"github.com/xraph/cortex"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/operator"
)

func (s *service) bindOptimizationCapabilities(d *dispatcher.Dispatcher, reader operator.Reader) error {
	return optimizationQuery(d, s, "optimization.capabilities", func(ctx context.Context, in operator.CapabilitiesInput) (opt.CapabilitySnapshot, error) {
		result, err := reader.Capabilities(ctx, in)
		if err != nil {
			return opt.CapabilitySnapshot{}, err
		}
		principal, ok := cortex.PrincipalFromContext(ctx).(dc.Principal)
		if !ok || principal.User == nil {
			return opt.CapabilitySnapshot{}, opt.ErrPolicyDenied
		}
		for action, allowed := range result.Permissions {
			permission := optimizationActionPermission(action)
			result.Permissions[action] = allowed && permission != "" && principal.User.HasScope("cortex."+permission)
		}
		return result, nil
	})
}

func optimizationActionPermission(action opt.Action) string {
	switch action {
	case opt.ActionRead:
		return "optimization.read"
	case opt.ActionCreate, opt.ActionRetain, opt.ActionRetryPublication, opt.ActionRevokeLesson:
		return "optimization.manage"
	case opt.ActionStart, opt.ActionCancel, opt.ActionResume:
		return "optimization.run"
	case opt.ActionActivate, opt.ActionRollback, opt.ActionDeactivate:
		return "optimization.activate"
	case opt.ActionReadArtifact:
		return "optimization.artifacts"
	default:
		return ""
	}
}
