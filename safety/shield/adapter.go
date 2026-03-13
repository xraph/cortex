// Package shield bridges the Shield safety engine to the Cortex safety.Scanner
// interface, following the same DI auto-discovery pattern as cortex/llm/nexus.
package shield

import (
	"context"

	"github.com/xraph/vessel"

	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/safety"

	"github.com/xraph/shield"
	shieldengine "github.com/xraph/shield/engine"
	"github.com/xraph/shield/scan"
)

var _ safety.Scanner = (*Adapter)(nil)

// Adapter implements safety.Scanner by delegating to a shield engine.
type Adapter struct {
	eng *shieldengine.Engine
}

// New creates a new Shield adapter from a shield engine.
func New(eng *shieldengine.Engine) *Adapter {
	return &Adapter{eng: eng}
}

// EngineOption returns an engine.Option that auto-discovers a Shield engine
// from the DI container and configures the cortex engine's safety scanner.
// If no shield engine is found, returns a no-op option (safe to always include).
func EngineOption(c vessel.Vessel) engine.Option {
	eng, err := vessel.Inject[*shieldengine.Engine](c)
	if err != nil {
		return func(_ *engine.Engine) error { return nil }
	}
	return engine.WithSafety(New(eng))
}

// ScanInput scans input content via shield.
func (a *Adapter) ScanInput(ctx context.Context, req *safety.ScanRequest) (*safety.ScanResult, error) {
	ctx = shield.WithTenant(ctx, req.TenantID)
	ctx = shield.WithApp(ctx, req.AppID)

	input := &scan.Input{
		Text:      req.Content,
		Direction: scan.DirectionInput,
		Context:   req.Metadata,
		Metadata: map[string]any{
			"agent_id": req.AgentID,
			"run_id":   req.RunID,
		},
	}

	result, err := a.eng.ScanInput(ctx, input)
	if err != nil {
		return nil, err
	}
	return fromShieldResult(result), nil
}

// ScanOutput scans output content via shield.
func (a *Adapter) ScanOutput(ctx context.Context, req *safety.ScanRequest) (*safety.ScanResult, error) {
	ctx = shield.WithTenant(ctx, req.TenantID)
	ctx = shield.WithApp(ctx, req.AppID)

	input := &scan.Input{
		Text:      req.Content,
		Direction: scan.DirectionOutput,
		Context:   req.Metadata,
		Metadata: map[string]any{
			"agent_id": req.AgentID,
			"run_id":   req.RunID,
		},
	}

	result, err := a.eng.ScanOutput(ctx, input)
	if err != nil {
		return nil, err
	}
	return fromShieldResult(result), nil
}

// fromShieldResult converts a shield scan.Result to a cortex safety.ScanResult.
func fromShieldResult(r *scan.Result) *safety.ScanResult {
	findings := make([]safety.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		findings = append(findings, safety.Finding{
			Layer:    f.Layer,
			Source:   f.Source,
			Severity: f.Severity,
			Message:  f.Message,
			Score:    f.Score,
		})
	}
	return &safety.ScanResult{
		Decision:    safety.Decision(r.Decision),
		Blocked:     r.Blocked,
		Findings:    findings,
		Redacted:    r.Redacted,
		PIICount:    r.PIICount,
		ProfileUsed: r.ProfileUsed,
		Duration:    r.Duration,
	}
}
