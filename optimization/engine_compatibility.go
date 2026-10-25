package optimization

import (
	"context"
	"errors"
	"strings"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
)

// EngineCompatibilityConfig binds the production engine and its deployed revision.
// Host must check current policy and any external dependencies in version.Sources.
// Returning compatible grants no execution authority.
type EngineCompatibilityConfig struct {
	Engine         *engine.Engine
	Host           Compatibility
	RuntimeVersion string
}

// EngineCompatibility checks current stored sources and runtime implementations
// after the host has checked external dependencies and policy.
type EngineCompatibility struct{ config EngineCompatibilityConfig }

// NewEngineCompatibility creates a read-only checker for a production engine.
// RuntimeVersion must identify the deployed runtime, not a caller-selected label.
func NewEngineCompatibility(config EngineCompatibilityConfig) (*EngineCompatibility, error) {
	if config.Engine == nil || config.Host == nil || strings.TrimSpace(config.RuntimeVersion) == "" || strings.TrimSpace(config.RuntimeVersion) != config.RuntimeVersion {
		return nil, ErrInvalidSpec
	}
	return &EngineCompatibility{config: config}, nil
}

// Check revalidates an execution version without executing models or tools.
// Host checks receive a detached version. A change during those checks is caught
// by the subsequent current-source snapshot and runtime-binding check.
func (c *EngineCompatibility) Check(ctx context.Context, version HarnessVersion) (CompatibilityReport, error) {
	var empty CompatibilityReport
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if version.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() || cortex.ScopeFromContext(ctx).IsZero() {
		return empty, ErrPolicyDenied
	}
	sealed, err := SealVersion(version)
	if err != nil || sealed.Digest != version.Digest || sealed.PolicyDigest != version.PolicyDigest {
		return empty, errors.Join(ErrStaleVersion, err)
	}
	if sealed.Kind != HarnessExecution || sealed.RuntimeVersion != c.config.RuntimeVersion {
		return empty, ErrStaleVersion
	}
	if sealed.Orchestration != nil {
		return empty, ErrUnavailable
	}
	detached, err := copyModelJSON(&sealed, 16<<20)
	if err != nil {
		return empty, err
	}
	report, err := c.config.Host.Check(copyScopeContext(ctx), *detached)
	if err != nil {
		return empty, err
	}
	if !report.Compatible || report.Version != (VersionRef{ID: sealed.ID, Digest: sealed.Digest}) || report.PolicyDigest != sealed.PolicyDigest {
		return empty, ErrStaleVersion
	}
	if err = c.config.Engine.CheckPreparedBindings(copyScopeContext(ctx), sealed.Prepared); err != nil {
		return empty, compatibilityError(err)
	}
	checked, err := c.config.Engine.CheckPreparedSources(copyScopeContext(ctx), sealed.Prepared)
	if err != nil {
		return empty, compatibilityError(err)
	}
	if checked == nil || checked.PreparedDigest != sealed.Prepared.Digest() || checked.SourceCount < 1 || checked.Snapshot.ID == "" || checked.Snapshot.Scope != sealed.Scope.Canonical() {
		return empty, ErrIncompleteEvidence
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return empty, contextErr
	}
	return CompatibilityReport{Version: VersionRef{ID: sealed.ID, Digest: sealed.Digest}, PolicyDigest: sealed.PolicyDigest, Compatible: true}, nil
}

func compatibilityError(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, cortex.ErrInvalidPrepared) || errors.Is(err, cortex.ErrPreparationConflict) {
		return errors.Join(ErrStaleVersion, err)
	}
	return errors.Join(ErrUnavailable, err)
}
