package engine

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

func runDeadline(ctx context.Context, cfg resolvedConfig) (context.Context, context.CancelFunc) {
	if cfg.Budget.Deadline.IsZero() {
		return context.WithCancel(ctx)
	}
	return context.WithDeadline(ctx, cfg.Budget.Deadline)
}

func (st *reactState) chargeTokens(reported int, cfg resolvedConfig, r *run.Run) {
	st.usage.ChargeTokens(reported, cfg.Budget)
	st.tokensUsed = st.usage.Tokens
	r.Usage = st.usage
	r.TokensUsed = st.tokensUsed
}

func (st *reactState) admitTools(count int, cfg resolvedConfig) execution.Limit {
	if limit := cfg.Budget.CheckTool(st.usage, time.Now()); limit != "" {
		return limit
	}
	if cfg.Budget.MaxToolCalls > 0 && count > cfg.Budget.MaxToolCalls-st.usage.ToolCalls {
		return execution.LimitTools
	}
	return ""
}

func (e *Engine) executionTools(ctx context.Context, subject cortex.Subject, cfg resolvedConfig, st *reactState) []llm.Tool {
	if st.preparedSnapshot == nil {
		return e.resolveTools(ctx, subject, cfg.Tools, cfg.ToolsRestricted)
	}
	// Decode copies from the immutable snapshot. A provider or authorizer may
	// retain or mutate its input; neither receives the next step's definitions.
	data, _ := json.Marshal(st.preparedSnapshot.Tools) //nolint:errcheck // validated JSON snapshot
	var defs []llm.Tool
	_ = decodePreparedJSON(data, &defs) //nolint:errcheck // round-trip of validated data
	if e.authorizer == nil {
		return defs
	}
	visible := e.authorizer.Visible(ctx, subject, defs)
	names := make([]string, 0, len(visible))
	for _, def := range visible {
		names = append(names, def.Name)
	}
	defs = nil
	_ = decodePreparedJSON(data, &defs) //nolint:errcheck // round-trip of validated data
	return filterByName(defs, names)
}

func (e *Engine) executePreparedTool(ctx context.Context, subject cortex.Subject, tc llm.ToolCall, st *reactState) (string, toolOutcome, suspension.SuspendReason) {
	if err := ctx.Err(); err != nil {
		e.extensions.EmitToolDenied(ctx, subject.RunID, tc.Name, err.Error())
		return jsonResult("error", err.Error()), outcomeDenied, ""
	}
	if st.preparedSnapshot != nil {
		allowed := false
		for _, def := range st.preparedSnapshot.Tools {
			if def.Name == tc.Name {
				allowed = true
				break
			}
		}
		if !allowed {
			message := "tool is outside the prepared allowlist: " + tc.Name
			e.extensions.EmitToolDenied(ctx, subject.RunID, tc.Name, message)
			return jsonResult("error", message), outcomeDenied, ""
		}
	}
	st.usage.ToolCalls++
	return e.executeTool(ctx, subject, tc)
}

func (st *reactState) failedAttempt(reported int, cfg resolvedConfig, r *run.Run) {
	st.stepIndex++
	st.usage.Steps = st.stepIndex
	st.chargeTokens(reported, cfg, r)
	r.StepCount = st.stepIndex
}

func streamReportedTokens(stream llm.Stream) int {
	if usage := stream.Usage(); usage != nil {
		return usage.TotalTokens
	}
	return 0
}
