package contract

import (
	"context"
	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"
)

func (s *service) bindStats(d *dispatcher.Dispatcher) error {
	return query(d, s, "agents.stats", func(ctx context.Context, in IDInput) (map[string]any, error) {
		v, err := id.ParseAgentID(in.ID)
		if err != nil {
			return nil, bad("Invalid agent ID")
		}
		ag, err := s.deps.Engine.GetAgent(ctx, v)
		if err != nil {
			return nil, err
		}
		ctx = cortex.WithScope(ctx, ag.Scope)
		f := &run.ListFilter{AgentID: in.ID, Exact: true}
		total, err := s.deps.Engine.CountRuns(ctx, f)
		if err != nil {
			return nil, err
		}
		f.State = run.StateCompleted
		completed, err := s.deps.Engine.CountRuns(ctx, f)
		if err != nil {
			return nil, err
		}
		f.State = ""
		f.Limit = 1
		latest, err := s.deps.Engine.ListRuns(ctx, f)
		if err != nil {
			return nil, err
		}
		var rate any
		if total > 0 {
			rate = float64(completed) / float64(total)
		}
		out := map[string]any{"total": total, "completed": completed, "success_rate": rate}
		if len(latest) > 0 {
			out["last_run"] = latest[0]
		}
		return out, nil
	})
}
