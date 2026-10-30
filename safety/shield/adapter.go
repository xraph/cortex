// Package shield bridges the Shield safety engine to Cortex: content scans
// through safety.Scanner and safety.PublicationGate, streamed model output a
// segment at a time through safety.IncrementalGate, and tool-call scans
// layered under the host's cortex.ToolAuthorizer (see NewToolAuthorizer).
//
// Every scan runs under a shield tenant taken from the run's scope by a
// cortex.TenantFunc, and under a shield app. A scan that cannot run (no
// tenant, no profile for the tenant, an invalid profile, input over
// shield's size limit) fails closed: content is not published and a tool
// call is denied. Errors carry only shield's sentinel, never the scanned
// text or the shield error's own detail.
package shield

import (
	"context"
	"errors"
	"fmt"

	"github.com/xraph/vessel"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/safety"

	"github.com/xraph/shield"
	shieldengine "github.com/xraph/shield/engine"
	"github.com/xraph/shield/scan"
)

var (
	_ safety.Scanner         = (*Adapter)(nil)
	_ safety.PublicationGate = (*Adapter)(nil)
	_ safety.IncrementalGate = (*Adapter)(nil)
)

// defaultUntrustedSources are the publication sources whose inbound text
// comes from outside the operator: tool results and errors, and retrieved
// knowledge.
var defaultUntrustedSources = []string{"tool_result", "tool_error", "retrieval"}

// shieldSentinels are the shield errors a failed scan may report. A scan
// error is returned as safety.ErrUnavailable joined with the first of these
// it matches, and nothing else of it.
var shieldSentinels = []error{
	shield.ErrTenantRequired,
	shield.ErrNoProfile,
	shield.ErrProfileInvalid,
	shield.ErrInputTooLarge,
	shield.ErrInvalidInput,
}

// matchedSentinel returns the shield sentinel err matches, or nil.
func matchedSentinel(err error) error {
	for _, s := range shieldSentinels {
		if errors.Is(err, s) {
			return s
		}
	}
	return nil
}

// unavailable is safety.ErrUnavailable joined with the shield sentinel err
// matches. The rest of err is dropped: shield's errors can name a profile,
// and a store error could carry anything.
func unavailable(err error) error {
	if s := matchedSentinel(err); s != nil {
		return fmt.Errorf("%w: %w", safety.ErrUnavailable, s)
	}
	return safety.ErrUnavailable
}

// Option configures an Adapter.
type Option func(*Adapter)

// WithTenant sets how the adapter names the shield tenant a scan runs
// under, from the scope on the context. The default is cortex.RootTenant,
// the value of the scope's first level. An error or an empty result fails
// the scan closed with shield.ErrTenantRequired.
func WithTenant(f cortex.TenantFunc) Option {
	return func(a *Adapter) {
		if f != nil {
			a.tenant = f
		}
	}
}

// WithApp sets the shield app every scan runs under, which together with
// the tenant selects the safety profiles. The default is the request's
// AppID, which the engine takes from the scope's "app" level or, without
// one, the canonical scope.
func WithApp(app string) Option {
	return func(a *Adapter) { a.app = app }
}

// WithUntrustedSources replaces the publication sources whose inbound text
// shield treats as untrusted. The default is "tool_result", "tool_error"
// and "retrieval". Shield quotes untrusted text it does not block, so the
// model reads it as data, and the gate publishes the quoted text.
func WithUntrustedSources(sources ...string) Option {
	return func(a *Adapter) {
		a.untrusted = make(map[string]bool, len(sources))
		for _, s := range sources {
			a.untrusted[s] = true
		}
	}
}

// WithSegmentLimit sets how many bytes of streamed model output a segment
// checker holds before it cuts a run of text with no sentence or line end
// at its last whitespace. The default is 2048. A value of zero or less
// keeps the default. See NewSegmentChecker.
func WithSegmentLimit(maxBytes int) Option {
	return func(a *Adapter) {
		if maxBytes > 0 {
			a.segmentLimit = maxBytes
		}
	}
}

// Adapter implements safety.Scanner, safety.PublicationGate and
// safety.IncrementalGate by delegating to a shield engine.
type Adapter struct {
	eng          *shieldengine.Engine
	tenant       cortex.TenantFunc
	app          string
	untrusted    map[string]bool
	segmentLimit int
}

// New creates a Shield adapter over eng.
func New(eng *shieldengine.Engine, opts ...Option) *Adapter {
	a := &Adapter{eng: eng, tenant: cortex.RootTenant, segmentLimit: defaultSegmentLimit}
	WithUntrustedSources(defaultUntrustedSources...)(a)
	for _, opt := range opts {
		opt(a)
	}
	return a
}

// EngineOption returns an engine.Option that auto-discovers a Shield engine
// from the DI container and installs the adapter as both the engine's
// safety scanner and its publication gate. If no shield engine is found, it
// returns a no-op option (safe to always include).
func EngineOption(c vessel.Vessel, opts ...Option) engine.Option {
	eng, err := vessel.Inject[*shieldengine.Engine](c)
	if err != nil {
		return func(_ *engine.Engine) error { return nil }
	}
	a := New(eng, opts...)
	return func(e *engine.Engine) error {
		if err := engine.WithSafety(a)(e); err != nil {
			return err
		}
		return engine.WithPublicationGate(a)(e)
	}
}

// ScanInput scans input content via shield.
func (a *Adapter) ScanInput(ctx context.Context, req *safety.ScanRequest) (*safety.ScanResult, error) {
	return a.scan(ctx, req, scan.DirectionInput, "", false)
}

// ScanOutput scans output content via shield.
func (a *Adapter) ScanOutput(ctx context.Context, req *safety.ScanRequest) (*safety.ScanResult, error) {
	return a.scan(ctx, req, scan.DirectionOutput, "", false)
}

// scanContext puts the shield tenant and app on ctx. The tenant comes from
// the scope through the TenantFunc, never from req.TenantID, which the
// engine fills with the canonical scope.
func (a *Adapter) scanContext(ctx context.Context, req *safety.ScanRequest) (context.Context, error) {
	tenant, err := a.tenant(ctx, cortex.ScopeFromContext(ctx))
	if err != nil || tenant == "" {
		return nil, unavailable(shield.ErrTenantRequired)
	}
	app := a.app
	if app == "" {
		app = req.AppID
	}
	return shield.WithApp(shield.WithTenant(ctx, tenant), app), nil
}

// scan runs one content scan. A result that names no profile is refused:
// it cannot show that any profile screened the text.
func (a *Adapter) scan(ctx context.Context, req *safety.ScanRequest, direction scan.Direction, source string, untrusted bool) (*safety.ScanResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if a == nil || a.eng == nil || req == nil {
		return nil, safety.ErrUnavailable
	}
	scanCtx, err := a.scanContext(ctx, req)
	if err != nil {
		return nil, err
	}

	input := &scan.Input{
		Text:      req.Content,
		Direction: direction,
		Context:   req.Metadata,
		Metadata: map[string]any{
			"agent_id": req.AgentID,
			"run_id":   req.RunID,
		},
		Profile:   req.ProfileName,
		Source:    source,
		Untrusted: untrusted,
	}

	var result *scan.Result
	if direction == scan.DirectionInput {
		result, err = a.eng.ScanInput(scanCtx, input)
	} else {
		result, err = a.eng.ScanOutput(scanCtx, input)
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, unavailable(err)
	}
	if result == nil || result.ProfileUsed == "" {
		return nil, safety.ErrUnavailable
	}
	return fromShieldResult(result), nil
}

// fromShieldResult converts a shield scan.Result to a cortex safety.ScanResult.
func fromShieldResult(r *scan.Result) *safety.ScanResult {
	if r == nil {
		return nil
	}
	findings := make([]safety.Finding, 0, len(r.Findings))
	for _, f := range r.Findings {
		if f == nil {
			continue
		}
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
