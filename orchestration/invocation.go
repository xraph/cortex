package orchestration

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// InvocationIntent binds one strategy slot to its exact request without storing
// private input. A retained intent is reconciliation evidence, never permission
// to reserve or execute the slot again.
type InvocationIntent struct {
	SchemaVersion   int                `json:"schema_version"`
	OrchestrationID id.OrchestrationID `json:"orchestration_id"`
	Scope           cortex.Scope       `json:"scope"`
	Slot            string             `json:"slot"`
	AgentName       string             `json:"agent_name"`
	RequestDigest   string             `json:"request_digest"`
}

// InvocationRecord retains admission intent and the identity returned by reserve.
// A zero RunID means reservation may not have run or its acknowledgment was lost.
type InvocationRecord struct {
	Intent InvocationIntent `json:"intent"`
	RunID  id.AgentRunID    `json:"run_id,omitempty"`
}

// InvocationStore admits a slot once and binds its reserved identity once.
// Even identical repeated writes return an error. Reads grant no dispatch rights.
// Every operation uses the caller's exact scope.
type InvocationStore interface {
	BeginOrchestrationInvocation(context.Context, InvocationIntent) error
	BindOrchestrationInvocation(context.Context, InvocationIntent, id.AgentRunID) error
	GetOrchestrationInvocation(context.Context, id.OrchestrationID, string) (InvocationRecord, error)
}

type invocationSlotKey struct{}

// InvocationSlot returns the strategy-defined position of this agent call.
// It distinguishes repeated calls to the same agent and is stable across replay
// of a frozen config and prior results. It is not caller authentication.
func InvocationSlot(ctx context.Context) string {
	slot, ok := ctx.Value(invocationSlotKey{}).(string)
	if !ok {
		return ""
	}
	return slot
}

// Freeze validates and detaches the bounded invocation identity.
func (in InvocationIntent) Freeze() (InvocationIntent, string, error) {
	if in.SchemaVersion != 1 || in.OrchestrationID.IsNil() || in.OrchestrationID.Prefix() != id.PrefixOrchestration || cortex.ValidateScope(in.Scope) != nil || !validInvocationSlot(in.Slot) || !localRosterName(in.AgentName) || !utf8.ValidString(in.AgentName) || !evidence.ValidDigest(in.RequestDigest) {
		return InvocationIntent{}, "", cortex.ErrInvalidState
	}
	data, err := json.Marshal(in)
	if err != nil || len(data) > 16<<10 {
		return InvocationIntent{}, "", cortex.ErrInvalidState
	}
	var frozen InvocationIntent
	if json.Unmarshal(data, &frozen) != nil {
		return InvocationIntent{}, "", cortex.ErrInvalidState
	}
	return frozen, evidence.Digest(data), nil
}

func validInvocationSlot(slot string) bool {
	if slot == "" || len(slot) > 128 || strings.HasPrefix(slot, "/") || strings.HasSuffix(slot, "/") || strings.Contains(slot, "//") {
		return false
	}
	for _, c := range slot {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '/' && c != '_' {
			return false
		}
	}
	return true
}

func invocationIntent(ctx context.Context, team id.OrchestrationID, name, input string, opts *RunOpts) (InvocationIntent, error) {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return InvocationIntent{}, err
	}
	// One digest binds every executable request field. No private prompt is copied
	// into the journal, and a changed override cannot reuse the same slot.
	data, err := json.Marshal(struct {
		Name, Input string
		Options     *RunOpts
	}{name, input, opts})
	if err != nil {
		return InvocationIntent{}, cortex.ErrInvalidState
	}
	in, _, err := (InvocationIntent{SchemaVersion: 1, OrchestrationID: team, Scope: scope, Slot: InvocationSlot(ctx), AgentName: name, RequestDigest: evidence.Digest(data)}).Freeze()
	return in, err
}
