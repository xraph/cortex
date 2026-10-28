// Package checkpoint defines the Checkpoint entity for human-in-the-loop control.
package checkpoint

import (
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

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
