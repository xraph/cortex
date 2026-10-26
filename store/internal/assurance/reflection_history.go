package assurance

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/id"
)

// ListReflections reads immutable decisions without granting execution authority.
func (s *Repository) ListReflections(ctx context.Context, runID id.AgentRunID, after int64, limit int) (cognitive.ReflectionPage, error) {
	out := cognitive.ReflectionPage{Items: []cognitive.ReflectionReceipt{}}
	if _, err := cortex.RequireScope(ctx); err != nil {
		return out, err
	}
	if runID.IsNil() || runID.Prefix() != id.PrefixAgentRun || after < 0 || limit < 1 || limit > 100 {
		return out, cognitive.ErrInvalidReflection
	}
	cursor := ""
	if after > 0 {
		cursor = reflectionID(runID, after)
	}
	rows, err := s.driver.Select(ctx, Query{Kind: kindReflection, RunID: runID.String(), After: cursor, Limit: limit + 1})
	if err != nil {
		return out, err
	}
	previous := after
	for _, row := range rows {
		receipt, decodeErr := decodeReflection(ctx, row)
		if decodeErr != nil {
			return cognitive.ReflectionPage{}, decodeErr
		}
		if receipt.Request.Snapshot.Binding.RunID != runID || receipt.State.Version <= previous {
			return cognitive.ReflectionPage{}, cognitive.ErrInvalidReflection
		}
		previous = receipt.State.Version
		if len(out.Items) == limit {
			out.After = out.Items[len(out.Items)-1].State.Version
			break
		}
		out.Items = append(out.Items, receipt)
	}
	return out, ctx.Err()
}
