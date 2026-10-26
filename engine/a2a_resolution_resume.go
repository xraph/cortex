package engine

import (
	"context"
	"errors"
	"slices"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/suspension"
)

const maxPendingQuestions = 256
const maxAgentReplyBytes = 4 << 20

func bindPendingQuestions(p pendingCalls) ([]suspension.PendingCall, error) {
	if len(p.calls) != len(p.asks) || len(p.calls) == 0 || len(p.calls) > maxPendingQuestions {
		return nil, a2a.ErrAskResolutionConflict
	}
	questions := make(map[string]string, len(p.asks))
	tokens := make(map[string]bool, len(p.asks))
	for _, ask := range p.asks {
		if ask == nil || ask.ToolCallID == "" || ask.ReplyWith == "" || questions[ask.ToolCallID] != "" || tokens[ask.ReplyWith] {
			return nil, a2a.ErrAskResolutionConflict
		}
		questions[ask.ToolCallID], tokens[ask.ReplyWith] = ask.ReplyWith, true
	}
	calls := slices.Clone(p.calls)
	for i := range calls {
		token := questions[calls[i].ID]
		if token == "" || calls[i].Name != toolAgentAsk {
			return nil, a2a.ErrAskResolutionConflict
		}
		calls[i].ReplyWith = token
		delete(questions, calls[i].ID)
	}
	return calls, nil
}

func checkAgentReplyVersion(paused *suspension.Suspension, in ResumeInput, source resumeSource) error {
	if source != resumeSourceAgentReply {
		return nil
	}
	if expected := in.agentReplyVersion; expected != nil {
		if paused == nil || paused.ID != expected.SuspensionID || paused.RunID != expected.RunID {
			return cortex.ErrNotSuspended
		}
	} else if paused != nil {
		for _, call := range paused.Pending {
			if call.ReplyWith != "" {
				return a2a.ErrAskResolutionConflict
			}
		}
	}
	return nil
}

// ResumeAgentResolution assembles the sealed results for this exact pause.
// Another pending question may still be waiting; its later answer will perform
// the same bounded read. The version claim arbitrates concurrent final answers.
func (a a2aResumer) ResumeAgentResolution(ctx context.Context, resolution a2a.AskResolution) error {
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return err
	}
	// This adapter belongs to the engine's global bus. Private prepared-team
	// buses need their own fixed partition and coordinator authority.
	if resolution.Ask.OrchestrationID != "" || resolution.Ask.Scope.Canonical() != scope.Canonical() {
		return a2a.ErrAskResolutionConflict
	}
	reader, ok := a.eng.store.(a2a.AskResolutionStore)
	if !ok {
		return a2a.ErrAskResolutionUnavailable
	}
	current, err := a.eng.store.GetRun(ctx, resolution.Ask.AskerRunID)
	if err != nil {
		return err
	}
	if current.State != run.StatePaused || current.Scope.Canonical() != scope.Canonical() {
		return cortex.ErrNotSuspended
	}
	paused, err := a.eng.store.GetSuspension(ctx, current.ID)
	if err != nil {
		return err
	}
	if paused.Reason != suspension.ReasonAgentReply || paused.Scope.Canonical() != scope.Canonical() || len(paused.Pending) == 0 || len(paused.Pending) > maxPendingQuestions {
		return a2a.ErrAskResolutionConflict
	}
	matched := false
	for _, call := range paused.Pending {
		if call.ReplyWith == resolution.Ask.ReplyWith && call.ID == resolution.Ask.ToolCallID && call.ReplyWith != "" {
			matched = true
		}
	}
	if !matched {
		return a2a.ErrAskResolutionConflict
	}
	in := ResumeInput{agentReplyVersion: &suspension.ClaimVersion{RunID: current.ID, SuspensionID: paused.ID, RunRevision: current.Revision}}
	complete := true
	resultBytes := 0
	seen := make(map[string]bool, len(paused.Pending))
	for _, call := range paused.Pending {
		if call.ReplyWith == "" || call.Name != toolAgentAsk || seen[call.ReplyWith] {
			return a2a.ErrAskResolutionConflict
		}
		seen[call.ReplyWith] = true
		retained, readErr := reader.GetAskResolution(ctx, call.ReplyWith)
		if errors.Is(readErr, a2a.ErrAskResolutionNotFound) {
			if call.ReplyWith == resolution.Ask.ReplyWith {
				return a2a.ErrAskResolutionConflict
			}
			complete = false
			continue
		}
		if readErr != nil {
			return readErr
		}
		if len(retained.Payload) > maxAgentReplyBytes-resultBytes {
			return safety.ErrLimit
		}
		resultBytes += len(retained.Payload)
		if retained.Ask.AskerRunID != current.ID || retained.Ask.ToolCallID != call.ID || retained.Ask.ReplyWith != call.ReplyWith || retained.Ask.Scope.Canonical() != scope.Canonical() || retained.Ask.OrchestrationID != "" || retained.Ask.ClaimedAt == nil {
			return a2a.ErrAskResolutionConflict
		}
		if call.ReplyWith == resolution.Ask.ReplyWith && (retained.ResolutionID != resolution.ResolutionID || retained.BindingDigest != resolution.BindingDigest || retained.OutcomeDigest != resolution.OutcomeDigest || retained.Payload != resolution.Payload) {
			return a2a.ErrAskResolutionConflict
		}
		in.ToolResults = append(in.ToolResults, ToolResult{ToolCallID: call.ID, Content: retained.Payload})
	}
	if !complete {
		return nil
	}
	_, err = a.eng.resume(ctx, current.ID, in, resumeSourceAgentReply)
	return err
}

var _ a2a.ResolutionResumer = a2aResumer{}
