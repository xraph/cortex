package engine

import (
	"context"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// PreparedTaskRunOptions supplies current dispatched ownership and a reserved
// child identity. These references do not grant authority. The host must use
// TaskRunAdmitter before creating the child and guard subsequent external calls.
type PreparedTaskRunOptions struct {
	Claim             coordination.ClaimToken
	DispatchDigest    string
	Assignment        coordination.Assignment
	ParentRunRevision int64
	RunID             id.AgentRunID
	SessionID         id.SessionID
	Dependencies      []coordination.AcceptedDependency
	Reason            string
}

// PrepareTaskRun scans input and history and persists the exact private execution
// inputs for a reserved child. It returns a detached creation request, but does
// not create a run, register its capture intent, emit lifecycle events or invoke
// generation/evaluation. Preserve this request for idempotent creation retries.
// Session resolution and authorized input-evidence storage may write records.
func (e *Engine) PrepareTaskRun(ctx context.Context, p *PreparedAgent, input string, opts PreparedTaskRunOptions) (coordination.TaskRunRequest, error) {
	if err := ctx.Err(); err != nil {
		return coordination.TaskRunRequest{}, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return coordination.TaskRunRequest{}, err
	}
	data, err := json.Marshal(opts)
	if err != nil || len(data) > 2<<20 {
		return coordination.TaskRunRequest{}, coordination.ErrInvalidClaim
	}
	var frozen PreparedTaskRunOptions
	if json.Unmarshal(data, &frozen) != nil {
		return coordination.TaskRunRequest{}, coordination.ErrInvalidClaim
	}
	opts = frozen
	h := opts.Assignment.Handoff
	if opts.Claim.Validate() != nil || opts.Assignment.Validate() != nil || opts.Assignment.State != coordination.AssignmentWorking || h.Purpose != coordination.HandoffWork || !evidence.ValidDigest(opts.DispatchDigest) || opts.ParentRunRevision < 1 || opts.RunID.IsNil() || opts.RunID.Prefix() != id.PrefixAgentRun || opts.RunID == h.ParentRunID || h.Scope.Canonical() != scope.Canonical() || opts.Claim.PlanID != h.PlanID || opts.Claim.TaskID != h.TaskID || opts.Claim.Worker != h.Receiver || opts.Claim.Fence != h.Fence || strings.TrimSpace(opts.Reason) == "" || len(opts.Reason) > 4096 || !utf8.ValidString(opts.Reason) {
		return coordination.TaskRunRequest{}, coordination.ErrInvalidClaim
	}
	snap, err := p.snapshot()
	if err != nil {
		return coordination.TaskRunRequest{}, err
	}
	if snap.Scope.Canonical() != scope.Canonical() || snap.Agent.ID.String() != opts.Claim.Worker.Agent || snap.Config.Verification == nil {
		return coordination.TaskRunRequest{}, cortex.ErrInvalidPrepared
	}
	b, ceiling := snap.Config.Budget, h.Authority.Budget
	if b.MaxSteps <= 0 || b.MaxTokens <= 0 || b.MaxToolCalls <= 0 || b.Deadline.IsZero() || b.MaxSteps > ceiling.MaxSteps || b.MaxTokens > ceiling.MaxTokens || b.MaxToolCalls > ceiling.MaxToolCalls || b.Deadline.After(ceiling.Deadline) {
		return coordination.TaskRunRequest{}, cortex.ErrInvalidPrepared
	}
	_, draft, err := e.prepareRun(ctx, p, input, PreparedRunOptions{SessionID: opts.SessionID, Purpose: run.PurposeNormal, Provenance: run.ExecutionProvenance{AssignmentDigest: h.Digest, ParentRunID: h.ParentRunID.String()}}, opts.RunID)
	if err != nil {
		return coordination.TaskRunRequest{}, err
	}
	child := *draft.run
	child.Provenance.TaskClaimID = opts.Claim.ClaimID.String()
	child.Entity, child.StartedAt = cortex.Entity{}, nil
	req := coordination.TaskRunRequest{SchemaVersion: 1, Claim: opts.Claim, DispatchDigest: opts.DispatchDigest, AssignmentRevision: opts.Assignment.Revision, AssignmentDigest: opts.Assignment.Digest, ParentRunRevision: opts.ParentRunRevision, Dependencies: opts.Dependencies, Run: child, Intent: draft.intent, Actor: opts.Claim.Worker.Principal, Reason: opts.Reason}
	result, _, err := req.Freeze()
	if err != nil {
		return coordination.TaskRunRequest{}, err
	}
	if err := ctx.Err(); err != nil {
		return coordination.TaskRunRequest{}, err
	}
	return result, nil
}
