// Package checkpoint defines the Checkpoint entity for human-in-the-loop control.
package checkpoint

import (
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

// MetadataKind is the metadata key that names what a checkpoint the engine
// opened is waiting on. It is set to KindDeferredModelCall on a checkpoint
// opened for a deferred model call, and absent on a tool approval.
const MetadataKind = "kind"

// KindDeferredModelCall is the MetadataKind of a checkpoint opened because
// the model client deferred a call (cortex.DeferredModelCallError). Such a
// checkpoint has no pending tool calls. An approval continues the run with
// the same model call; a rejection stops the run with the decision's
// reason. A decision that carries Calls or AmendedArguments is refused
// with cortex.ErrInvalidDecision.
const KindDeferredModelCall = "deferred_model_call"

// MetadataDeferralKind is the metadata key that carries
// cortex.DeferredModelCallError.Kind on a deferred model call's checkpoint.
const MetadataDeferralKind = "deferral_kind"

// Decision represents the resolution of a checkpoint.
type Decision struct {
	Approved  bool      `json:"approved"`
	DecidedBy string    `json:"decided_by,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	DecidedAt time.Time `json:"decided_at"`
	// DecidedByPrincipal is the decider's principal, as the engine's
	// cortex.PrincipalCodec encoded it. ResolveCheckpoint fills it from
	// the context and overwrites whatever the caller supplied. It records
	// who decided; the run itself continues under its own principal.
	//
	// It is serialized because the SQL stores persist a decision as JSON.
	// Anything that serves a checkpoint to a reader goes through
	// Checkpoint.Public, which drops it.
	DecidedByPrincipal []byte `json:"decided_by_principal,omitempty"`
	// AmendedArguments, on an approval, are the arguments the suspended
	// call runs with instead of the model's. They replace the whole
	// argument object, nothing is merged. They must be a JSON object, and
	// the suspension must hold exactly one pending call, or the decision
	// is refused with cortex.ErrInvalidDecision. The engine does not
	// validate them against the tool's schema; that is the caller's job.
	// A rejection ignores them and does not record them.
	AmendedArguments json.RawMessage `json:"amended_arguments,omitempty"`
	// Calls decides each pending tool call on its own, keyed by the
	// pending call's id. When it is set, Approved is derived (true when
	// any call is approved), AmendedArguments must be empty, and every
	// pending call must have an entry and every entry must name a
	// pending call, or the decision is refused with
	// cortex.ErrInvalidDecision. A rejected call's Reason is what the
	// model reads for that call. When it is empty, Approved and
	// AmendedArguments decide every pending call together.
	Calls map[string]CallDecision `json:"calls,omitempty"`
}

// CallDecision is the outcome of one pending tool call within a Decision.
type CallDecision struct {
	// Approved runs the call. A call that is not approved is rejected
	// and answered with Reason.
	Approved bool `json:"approved"`
	// AmendedArguments, on an approved call, replace the whole argument
	// object the call runs with. They must be a JSON object. A rejected
	// call ignores them.
	AmendedArguments json.RawMessage `json:"amended_arguments,omitempty"`
	// Reason is what the model reads when the call is rejected.
	Reason string `json:"reason,omitempty"`
}

// Checkpoint represents a point where a run pauses for approval.
type Checkpoint struct {
	cortex.Entity
	ID        id.CheckpointID `json:"id"`
	RunID     id.AgentRunID   `json:"run_id"`
	AgentID   id.AgentID      `json:"agent_id"`
	Scope     cortex.Scope    `json:"scope"`
	Reason    string          `json:"reason"`
	StepIndex int             `json:"step_index"`
	State     string          `json:"state"`
	Decision  *Decision       `json:"decision,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
}

// Public is a copy of c for a reader outside the engine, such as an API
// response or a dashboard. It drops Decision.DecidedByPrincipal: the
// store keeps the decider's codec bytes as the record of who decided,
// and they are the host's own, so no reader of the checkpoint gets them.
func (c *Checkpoint) Public() *Checkpoint {
	if c == nil {
		return nil
	}
	out := *c
	if c.Decision != nil {
		decision := *c.Decision
		decision.DecidedByPrincipal = nil
		out.Decision = &decision
	}
	return &out
}

// PublicList applies Public to every checkpoint in cps.
func PublicList(cps []*Checkpoint) []*Checkpoint {
	if cps == nil {
		return nil
	}
	out := make([]*Checkpoint, len(cps))
	for i, cp := range cps {
		out[i] = cp.Public()
	}
	return out
}
