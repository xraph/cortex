package contract

import (
	"context"
	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"
	"slices"
)

func (s *service) bindToolUsage(d *dispatcher.Dispatcher) error {
	return query(d, s, "tools.usage", func(ctx context.Context, in struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}) (map[string]any, error) { v, err := id.ParseAgentID(in.ID); if err != nil {
		return nil, bad("Invalid agent ID")
	}; ag, err := s.deps.Engine.GetAgent(ctx, v); if err != nil {
		return nil, err
	}; ctx = cortex.WithScope(ctx, ag.Scope); tools, err := s.deps.Engine.VisibleTools(ctx, v); if err != nil {
		return nil, err
	}; if !slices.ContainsFunc(tools, func(t llm.Tool) bool { return t.Name == in.Name }) {
		return nil, denied("Tool is not visible to this agent")
	}; calls, failures := 0, 0; for offset := 0; ; offset += 100 {
		rows, err := s.deps.Engine.ListRuns(ctx, &run.ListFilter{AgentID: in.ID, Exact: true, Limit: 100, Offset: offset})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			steps, err := s.deps.Engine.ListSteps(ctx, r.ID)
			if err != nil {
				return nil, err
			}
			for _, step := range steps {
				items, err := s.deps.Engine.ListToolCalls(ctx, step.ID)
				if err != nil {
					return nil, err
				}
				for _, item := range items {
					if item.ToolName == in.Name {
						calls++
						if item.Error != "" {
							failures++
						}
					}
				}
			}
		}
		if len(rows) < 100 {
			break
		}
		if err = ctx.Err(); err != nil {
			return nil, err
		}
	}; agents := []map[string]any{}; skills := []map[string]any{}; for offset := 0; ; offset += 100 {
		rows, err := s.deps.Engine.ListAgents(ctx, &agent.ListFilter{Exact: true, Limit: 100, Offset: offset})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			if slices.Contains(r.Tools, in.Name) {
				agents = append(agents, map[string]any{"id": r.ID.String(), "name": r.Name})
			}
		}
		if len(rows) < 100 {
			break
		}
	}; for offset := 0; ; offset += 100 {
		rows, err := s.deps.Engine.ListSkills(ctx, &skill.ListFilter{Exact: true, Limit: 100, Offset: offset})
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			for _, binding := range r.Tools {
				if binding.ToolName == in.Name {
					skills = append(skills, map[string]any{"id": r.ID.String(), "name": r.Name, "binding": binding})
				}
			}
		}
		if len(rows) < 100 {
			break
		}
	}; var rate any; if calls > 0 {
		rate = float64(failures) / float64(calls)
	}; return map[string]any{"calls": calls, "errors": failures, "error_rate": rate, "agents": agents, "skills": skills}, nil })
}
