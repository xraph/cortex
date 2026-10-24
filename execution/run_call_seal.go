package execution

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

var (
	ErrRunCallsSealed     = cortex.ErrRunCallsSealed
	ErrRunCallUnavailable = cortex.ErrRunCallUnavailable
)

// RunCallSealRequest closes admission to one exact run journal. The host
// authenticates Actor and authorizes the closure under its current authority
// fence. Closing an empty journal is valid: a delayed worker must not start it.
type RunCallSealRequest struct {
	SchemaVersion int           `json:"schema_version"`
	Scope         cortex.Scope  `json:"scope"`
	RunID         id.AgentRunID `json:"run_id"`
	Actor         string        `json:"actor"`
	Reason        string        `json:"reason"`
}

// Freeze binds and detaches a bounded journal closure request.
func (r RunCallSealRequest) Freeze() (RunCallSealRequest, string, error) {
	if r.SchemaVersion != 1 || cortex.ValidateScope(r.Scope) != nil || r.RunID.IsNil() || r.RunID.Prefix() != id.PrefixAgentRun || !operationText(r.Actor, 512) || !operationText(r.Reason, 4096) {
		return RunCallSealRequest{}, "", ErrInvalidRunCall
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 16<<10 {
		return RunCallSealRequest{}, "", ErrInvalidRunCall
	}
	var frozen RunCallSealRequest
	if json.Unmarshal(data, &frozen) != nil {
		return RunCallSealRequest{}, "", ErrInvalidRunCall
	}
	return frozen, evidence.Digest(data), nil
}

// RunCallSeal is a permanent admission barrier, not proof of provider completion.
// Already admitted calls may still finish. Their late returns and external proof
// remain writable. No closure grants a retry, result acceptance or usage refund.
type RunCallSeal struct {
	Request       RunCallSealRequest `json:"request"`
	RequestDigest string             `json:"request_digest"`
	At            time.Time          `json:"at"`
}

// Validate checks immutable closure metadata, not host authorization.
func (s RunCallSeal) Validate() error {
	_, digest, err := s.Request.Freeze()
	if err != nil || digest != s.RequestDigest || s.At.IsZero() {
		return ErrInvalidRunCall
	}
	return nil
}

// RunCallSealStore serializes closure with every call admission in the exact
// run and scope. After an acknowledged seal, bounded journal scans have stable
// membership. Historical BeginRunCall attempts also return ErrRunCallsSealed.
// All writers must implement this protocol; older writers cannot be fenced by it.
// Reads return nil for unsealed runs. Retain seals at least as long as call rows
// and possible delayed workers. There is no reopen operation.
type RunCallSealStore interface {
	RunCallStore
	SealRunCalls(context.Context, RunCallSealRequest) (RunCallSeal, error)
	GetRunCallSeal(context.Context, id.AgentRunID) (*RunCallSeal, error)
}
