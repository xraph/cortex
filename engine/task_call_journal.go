package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/internal/hostport"
	"github.com/xraph/cortex/llm"
)

type taskCallIndexKey struct{}

func withTaskCallIndex(ctx context.Context, index int) context.Context {
	return context.WithValue(ctx, taskCallIndexKey{}, index)
}

// Sync and stream generation share a slot; switching modes cannot retry a call.
// Tool slots use the originating generation index, including approval resumes.
func taskCallOperation(ctx context.Context, kind TaskCallKind, payload any) (string, error) {
	index, ok := ctx.Value(taskCallIndexKey{}).(int)
	if !ok || index < 0 {
		return "", cortex.ErrTaskCallUnavailable
	}
	slot := struct {
		Index  int
		Kind   string
		ToolID string
	}{Index: index, Kind: "generation"}
	if kind == TaskCallTool {
		call, valid := payload.(llm.ToolCall)
		if !valid || call.ID == "" {
			return "", cortex.ErrTaskCallUnavailable
		}
		slot.Kind, slot.ToolID = "tool", call.ID
	}
	data, err := json.Marshal(slot)
	if err != nil {
		return "", cortex.ErrTaskCallUnavailable
	}
	return evidence.Digest(data), nil
}

func (e *Engine) journalTaskCall(ctx context.Context, request TaskCallRequest, digest string, expiry time.Time) (context.Context, error) {
	journal, ok := e.store.(execution.RunCallStore)
	if !ok || hostport.IsNil(journal) {
		return nil, cortex.ErrTaskCallUnavailable
	}
	metadata := execution.RunCallRequest{SchemaVersion: 1, ID: request.ID, Scope: request.Scope, RunID: request.RunID, AgentID: request.AgentID, ClaimID: request.ClaimID, ParentRunID: request.ParentRunID, Kind: execution.RunCallKind(request.Kind), OperationDigest: request.OperationDigest, RequestDigest: digest, PreparedDigest: request.PreparedDigest, AssignmentDigest: request.AssignmentDigest, Deadline: expiry}
	offered, _, err := metadata.Freeze()
	if err != nil {
		return nil, cortex.ErrTaskCallUnavailable
	}
	fresh, err := journal.BeginRunCall(ctx, offered)
	if err != nil || !fresh {
		return nil, cortex.ErrTaskCallUncertain
	}
	if err = ctx.Err(); err != nil {
		return nil, cortex.ErrTaskCallUncertain
	}
	return execution.WithRunCallContext(ctx, metadata), nil
}

func (e *Engine) recordTaskCallReturn(ctx context.Context, resultDigest string, usage *llm.Usage) error {
	metadata, ok := execution.RunCallFromContext(ctx)
	if !ok || ctx.Err() != nil {
		return cortex.ErrTaskCallUncertain
	}
	journal, ok := e.store.(execution.RunCallStore)
	if !ok || hostport.IsNil(journal) {
		return cortex.ErrTaskCallUncertain
	}
	_, digest, err := metadata.Freeze()
	if err != nil {
		return cortex.ErrTaskCallUncertain
	}
	completion := execution.RunCallCompletion{SchemaVersion: 1, RequestDigest: digest, ResultDigest: resultDigest}
	if usage != nil {
		completion.TokensReported = true
		completion.ReportedTokens = int64(usage.TotalTokens)
	}
	if completion.Validate(metadata) != nil {
		return cortex.ErrTaskCallUncertain
	}
	if journal.CompleteRunCall(ctx, metadata, completion) != nil || ctx.Err() != nil {
		return cortex.ErrTaskCallUncertain
	}
	return nil
}

func (e *Engine) recordTaskCallValue(ctx context.Context, value any, usage *llm.Usage) error {
	data, err := json.Marshal(value)
	if err != nil {
		return cortex.ErrTaskCallUncertain
	}
	return e.recordTaskCallReturn(ctx, evidence.Digest(data), usage)
}
