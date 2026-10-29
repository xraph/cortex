package shield

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"

	"github.com/xraph/shield"
	shieldengine "github.com/xraph/shield/engine"
	"github.com/xraph/shield/scan"
)

// ErrToolCallBlocked is the denial a tool call gets when shield blocks it.
// The denial adds shield's reason, which is the message of the finding
// that blocked the call and never holds argument content.
var ErrToolCallBlocked = errors.New("shield: tool call blocked")

// ErrToolScanUnavailable is the denial a tool call gets when shield cannot
// scan it: no tenant, no profile, an invalid profile, arguments that are not
// JSON or that are too large. The denial is joined with the shield sentinel
// the scan failed with, and nothing else of the scan's error.
var ErrToolScanUnavailable = errors.New("shield: tool-call safety scan unavailable")

// ToolCallOptions configures NewToolAuthorizer.
type ToolCallOptions struct {
	// RiskClass is the host's own class for a call, one of shield's
	// presets.Risk* strings for the InfraAgent tools profile. Nil, or an
	// empty answer, is shield's unknown class, which that profile blocks.
	RiskClass func(ctx context.Context, s cortex.Subject, call llm.ToolCall) string
	// Profile names the shield profile tool calls are scanned under, such
	// as presets.InfraAgentTools. Empty uses shield's default profile.
	Profile string
	// ApprovalTTL is the decision deadline on an approval only shield
	// asked for. Zero uses the engine's suspension TTL. An approval the
	// host asked for keeps the host's own TTL.
	ApprovalTTL time.Duration
	// Tenant names the shield tenant from the run's scope. Nil is
	// cortex.RootTenant.
	Tenant cortex.TenantFunc
	// App is the shield app. Empty is the scope's "app" level or, without
	// one, the canonical scope, the same app the engine gives content
	// scans.
	App string
}

// toolAuthorizer layers shield's tool-call scan under a host authorizer.
type toolAuthorizer struct {
	eng  *shieldengine.Engine
	host cortex.ToolAuthorizer
	opts ToolCallOptions
}

// NewToolAuthorizer layers shield's tool-call scan under the host's
// authorizer and combines the two with scan.Stricter, so it can deny or
// require approval but never allow what the host refused.
//
// The host answers first. A host denial is returned as it is, and shield is
// not asked. Otherwise the call is scanned and the stricter answer wins: a
// shield block denies the call with ErrToolCallBlocked, and a scan that
// cannot run denies it with ErrToolScanUnavailable. An approval the host
// asked for comes back as the host's own error, so its reason and TTL hold.
// One only shield asked for is a cortex.ApprovalRequired whose reason starts
// "shield: " and whose TTL is opts.ApprovalTTL.
//
// A nil host allows every call and shows every tool, so shield decides
// alone. Visible always defers to the host: shield only gates calls.
func NewToolAuthorizer(eng *shieldengine.Engine, host cortex.ToolAuthorizer, opts ToolCallOptions) cortex.ToolAuthorizer {
	if opts.Tenant == nil {
		opts.Tenant = cortex.RootTenant
	}
	return &toolAuthorizer{eng: eng, host: host, opts: opts}
}

// Visible returns what the host shows, or every tool without a host.
func (t *toolAuthorizer) Visible(ctx context.Context, s cortex.Subject, tools []llm.Tool) []llm.Tool {
	if t.host == nil {
		return tools
	}
	return t.host.Visible(ctx, s, tools)
}

// Authorize gates one call, host first and then shield.
func (t *toolAuthorizer) Authorize(ctx context.Context, s cortex.Subject, call llm.ToolCall) error {
	hostDecision := scan.DecisionAllow
	var hostErr error
	if t.host != nil {
		hostErr = t.host.Authorize(ctx, s, call)
		switch {
		case hostErr == nil:
		case errors.Is(hostErr, cortex.ErrRequiresApproval):
			hostDecision = scan.DecisionRequireApproval
		default:
			return hostErr
		}
	}

	result, scanErr := t.scan(ctx, s, call)
	if scanErr != nil {
		return scanErr
	}

	switch scan.Stricter(hostDecision, result.Decision) {
	case scan.DecisionAllow:
		return nil
	case scan.DecisionRequireApproval:
		if hostDecision == scan.DecisionRequireApproval {
			return hostErr
		}
		return cortex.ApprovalRequired{Reason: "shield: " + reasonOr(result, "the call needs approval"), TTL: t.opts.ApprovalTTL}
	default:
		// Block, and anything a tool call should never get (flag,
		// redact), refuses the call.
		if reason := result.Reason(); reason != "" {
			return fmt.Errorf("%w: %s", ErrToolCallBlocked, reason)
		}
		return ErrToolCallBlocked
	}
}

// reasonOr returns the result's reason, or fallback when it gives none.
func reasonOr(r *scan.Result, fallback string) string {
	if reason := r.Reason(); reason != "" {
		return reason
	}
	return fallback
}

// scan runs shield's tool-call scan. A scan that cannot run, or that names
// no profile, is ErrToolScanUnavailable.
func (t *toolAuthorizer) scan(ctx context.Context, s cortex.Subject, call llm.ToolCall) (*scan.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if t.eng == nil {
		return nil, ErrToolScanUnavailable
	}
	scope := s.Scope
	if scope.IsZero() {
		scope = cortex.ScopeFromContext(ctx)
	}
	tenant, err := t.opts.Tenant(ctx, scope)
	if err != nil || tenant == "" {
		return nil, fmt.Errorf("%w: %w", ErrToolScanUnavailable, shield.ErrTenantRequired)
	}
	app := t.opts.App
	if app == "" {
		app = scopeApp(scope)
	}
	scanCtx := shield.WithApp(shield.WithTenant(ctx, tenant), app)

	in := &scan.ToolCallInput{
		Tool:     call.Name,
		Profile:  t.opts.Profile,
		Metadata: map[string]string{"run_id": s.RunID.String(), "agent_id": s.AgentID.String()},
	}
	if call.Arguments != "" {
		in.Arguments = json.RawMessage(call.Arguments)
	}
	if t.opts.RiskClass != nil {
		in.RiskClass = t.opts.RiskClass(ctx, s, call)
	}

	result, err := t.eng.ScanToolCall(scanCtx, in)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		if sentinel := matchedSentinel(err); sentinel != nil {
			return nil, fmt.Errorf("%w: %w", ErrToolScanUnavailable, sentinel)
		}
		return nil, ErrToolScanUnavailable
	}
	if result == nil || result.ProfileUsed == "" {
		return nil, ErrToolScanUnavailable
	}
	return result, nil
}

// scopeApp is the app the engine gives a content scan: the scope's "app"
// level, or the canonical scope without one.
func scopeApp(s cortex.Scope) string {
	if v, ok := s.Get("app"); ok {
		return v
	}
	return s.Canonical()
}
