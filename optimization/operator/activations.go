package operator

import (
	"context"
	"crypto/sha256"

	"go.jetify.com/typeid/v2"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	opt "github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// ActivationRouteInput identifies the production route and its reviewed generation.
// The command ID is derived from the transport key, never from payload fields.
type ActivationRouteInput struct {
	AgentID            string          `json:"agent_id"`
	Channel            string          `json:"channel,omitempty"`
	Kind               opt.HarnessKind `json:"kind,omitempty"`
	ExpectedGeneration int64           `json:"expected_generation" optional:"true"`
	Reason             string          `json:"reason"`
}

// ActivateInput names immutable version and decision evidence for a route change.
type ActivateInput struct {
	ActivationRouteInput
	VersionID     string `json:"version_id"`
	VersionDigest string `json:"version_digest"`
	DecisionID    string `json:"decision_id"`
}

// RollbackInput selects an existing activation for current policy revalidation.
type RollbackInput struct {
	ActivationRouteInput
	TargetID string `json:"target_id"`
}

// Activate records a reviewed route change. Retries return its immutable receipt,
// not a new generation or a claim that this receipt is still the current route.
func (c Commands) Activate(ctx context.Context, in ActivateInput, key string) (*opt.Activation, error) {
	route, err := c.activationRoute(ctx, in.ActivationRouteInput, key)
	if err != nil {
		return nil, err
	}
	version, err := opt.ParseID(in.VersionID, opt.PrefixVersion)
	if err != nil {
		return nil, err
	}
	decision, err := opt.ParseID(in.DecisionID, opt.PrefixDecision)
	if err != nil {
		return nil, err
	}
	return c.Optimizer.Activate(ctx, opt.ActivationRequest{ID: route.ID, AgentID: route.AgentID, Channel: route.Channel, Kind: route.Kind, ExpectedGeneration: route.ExpectedGeneration, Reason: route.Reason, Version: opt.VersionRef{ID: version, Digest: in.VersionDigest}, DecisionID: decision})
}

// Rollback rechecks current authority, evidence and compatibility before restoring
// a historical version. Its receipt key shares the activation command namespace.
func (c Commands) Rollback(ctx context.Context, in RollbackInput, key string) (*opt.Activation, error) {
	route, err := c.activationRoute(ctx, in.ActivationRouteInput, key)
	if err != nil {
		return nil, err
	}
	target, err := opt.ParseID(in.TargetID, opt.PrefixActivation)
	if err != nil {
		return nil, err
	}
	return c.Optimizer.Rollback(ctx, opt.RollbackRequest{ID: route.ID, AgentID: route.AgentID, Channel: route.Channel, Kind: route.Kind, ExpectedGeneration: route.ExpectedGeneration, Reason: route.Reason, TargetID: target})
}

// Deactivate disables the current route without requiring its old evidence to
// remain available. A retry cannot disable a subsequently restored generation.
func (c Commands) Deactivate(ctx context.Context, in ActivationRouteInput, key string) (*opt.Activation, error) {
	route, err := c.activationRoute(ctx, in, key)
	if err != nil {
		return nil, err
	}
	return c.Optimizer.Deactivate(ctx, route)
}

func (c Commands) activationRoute(ctx context.Context, in ActivationRouteInput, key string) (opt.DeactivationRequest, error) {
	var out opt.DeactivationRequest
	if c.Optimizer == nil {
		return out, opt.ErrUnavailable
	}
	if in.ExpectedGeneration < 0 {
		return out, opt.ErrInvalidSpec
	}
	command, err := activationCommandID(ctx, key)
	if err != nil {
		return out, err
	}
	owner, err := agentID(in.AgentID, true)
	if err != nil {
		return out, err
	}
	channel, err := opt.ActivationChannel(in.Channel)
	if err != nil {
		return out, err
	}
	kind, err := opt.ActivationKind(in.Kind)
	if err != nil {
		return out, err
	}
	return opt.DeactivationRequest{ID: command, AgentID: owner, Channel: channel, Kind: kind, ExpectedGeneration: in.ExpectedGeneration, Reason: in.Reason}, nil
}

func activationCommandID(ctx context.Context, key string) (id.ID, error) {
	if !opt.ValidOpaqueKey(key) {
		return id.Nil, opt.ErrInvalidSpec
	}
	if cortex.ScopeFromContext(ctx).IsZero() {
		return id.Nil, cortex.ErrNoScope
	}
	scope, err := opt.ScopeKey(cortex.ScopeFromContext(ctx))
	if err != nil {
		return id.Nil, err
	}
	// Bind only the scope and opaque key here. Action, actor, route and payload
	// are checked by the durable receipt, so changed requests collide and fail.
	encoded, err := canonical.Marshal([]string{"cortex.optimization.activation-command.v1", scope, key})
	if err != nil {
		return id.Nil, err
	}
	digest := sha256.Sum256(encoded)
	// UUIDv8 reserves the version and variant bits for this hash-derived identity.
	digest[6] = (digest[6] & 0x0f) | 0x80
	digest[8] = (digest[8] & 0x3f) | 0x80
	value, err := typeid.FromBytes(string(opt.PrefixActivation), digest[:16])
	if err != nil {
		return id.Nil, err
	}
	return id.Parse(value.String())
}
