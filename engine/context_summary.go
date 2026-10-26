package engine

import (
	"context"
	"errors"
	"strconv"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
)

// configureContextSummary requires private original-history capture. A legacy
// context policy remains usable without automatic summaries or verification.
func (e *Engine) configureContextSummary() error {
	if e.contextBudget != nil && e.contextBudget.Policy.Summary.Enabled() && e.requiredVerification == nil {
		return cortex.ErrContextUnavailable
	}
	return nil
}

func (e *Engine) summarizeContextHistory(ctx context.Context, snap *preparedSnapshot, current *run.Run, record *cognitive.ContextRecord, original, selected []llm.Message) ([]llm.Message, error) {
	if !record.Policy.Summary.Enabled() {
		return selected, nil
	}
	if snap.Config.Verification == nil {
		return nil, cortex.ErrContextUnavailable
	}
	record.SchemaVersion = 2
	var omitted []int
	for _, item := range record.Selection.Omitted {
		if !item.Optional {
			return nil, cognitive.ErrInvalidContext
		}
		index, err := strconv.Atoi(item.Key)
		if err != nil {
			return nil, cognitive.ErrInvalidContext
		}
		omitted = append(omitted, index)
	}
	summary, text, err := cognitive.SummarizeHistory(record.Policy.Summary, original, omitted)
	if err != nil {
		return nil, err
	}
	if summary == nil {
		return selected, nil
	}
	candidate := append([]llm.Message{{Role: "user", Content: text}}, selected...)
	base := contextBaseRequest(snap, current.Input)
	base.Messages = append(cloneExecutionMessages(candidate), llm.Message{Role: "user", Content: current.Input})
	if err = e.admitContextRequest(ctx, snap.Config.Context, &base); err != nil {
		if errors.Is(err, cognitive.ErrContextBudgetExceeded) {
			return selected, nil
		}
		return nil, err
	}
	record.Summary = summary
	return candidate, nil
}
