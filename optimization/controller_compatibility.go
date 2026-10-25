package optimization

import (
	"context"

	"github.com/xraph/cortex"
)

// ControllerCompatibilityConfig binds activation to the same factory that
// executes controllers. Host checks current policy and external provider routes.
type ControllerCompatibilityConfig struct {
	Controllers *ControllerFactory
	Host        Compatibility
}

// ControllerCompatibility verifies frozen controller bindings without model calls.
type ControllerCompatibility struct{ config ControllerCompatibilityConfig }

// NewControllerCompatibility creates a checker for reviewed controller routes.
func NewControllerCompatibility(config ControllerCompatibilityConfig) (*ControllerCompatibility, error) {
	if config.Controllers == nil || config.Host == nil {
		return nil, ErrInvalidSpec
	}
	return &ControllerCompatibility{config: config}, nil
}

// Check verifies scope, immutable content, supported tools and registered model
// revision before consulting the host's current policy and provider bindings.
func (c *ControllerCompatibility) Check(ctx context.Context, version HarnessVersion) (CompatibilityReport, error) {
	var empty CompatibilityReport
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	scope := cortex.ScopeFromContext(ctx)
	if scope.IsZero() || version.Scope.Canonical() != scope.Canonical() {
		return empty, ErrPolicyDenied
	}
	if _, _, err := c.config.Controllers.controllerBinding(version); err != nil {
		return empty, err
	}
	detached, err := copyModelJSON(&version, 16<<20)
	if err != nil {
		return empty, err
	}
	report, err := c.config.Host.Check(copyScopeContext(ctx), *detached)
	if err != nil {
		return empty, err
	}
	if !report.Compatible || report.Version != (VersionRef{ID: version.ID, Digest: version.Digest}) || report.PolicyDigest != version.PolicyDigest {
		return empty, ErrStaleVersion
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	return report, nil
}
