package contract

import (
	"context"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/trait"
	dc "github.com/xraph/forge/extensions/dashboard/contract"
	"github.com/xraph/forge/extensions/dashboard/contract/dispatcher"
)

func (s *service) bindOperations(d *dispatcher.Dispatcher) error {
	return dispatcher.RegisterQuery(d, ContributorName, "overview.stats", 1, func(ctx context.Context, _ struct{}, p dc.Principal) (map[string]int64, error) {
		ctx, err := s.context(ctx, p, "read")
		if err != nil {
			return nil, err
		}
		counts := map[string]int64{}
		for key, fn := range map[string]func() (int64, error){
			"agents":    func() (int64, error) { return s.deps.Engine.CountAgents(ctx, &agent.ListFilter{}) },
			"personas":  func() (int64, error) { return s.deps.Engine.CountPersonas(ctx, &persona.ListFilter{}) },
			"skills":    func() (int64, error) { return s.deps.Engine.CountSkills(ctx, &skill.ListFilter{}) },
			"traits":    func() (int64, error) { return s.deps.Engine.CountTraits(ctx, &trait.ListFilter{}) },
			"behaviors": func() (int64, error) { return s.deps.Engine.CountBehaviors(ctx, &behavior.ListFilter{}) },
			"orchestrations": func() (int64, error) {
				return s.deps.Engine.CountOrchestrations(ctx, &orchestration.ConfigListFilter{})
			},
			"runs":        func() (int64, error) { return s.deps.Engine.CountRuns(ctx, &run.ListFilter{}) },
			"checkpoints": func() (int64, error) { return s.deps.Engine.CountPendingCheckpoints(ctx, &checkpoint.ListFilter{}) },
		} {
			n, err := fn()
			if err != nil {
				return nil, mapError(err)
			}
			counts[key] = n
		}
		return counts, nil
	})
}
