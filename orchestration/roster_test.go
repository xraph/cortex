package orchestration_test

import (
	"reflect"
	"testing"

	"github.com/xraph/cortex/orchestration"
)

func TestExecutionRosterPinsStrategyAgents(t *testing.T) {
	for _, strategy := range []string{orchestration.StrategySequential, orchestration.StrategyParallel, orchestration.StrategyRouter, orchestration.StrategyHierarchical, orchestration.StrategyDebate} {
		t.Run(strategy, func(t *testing.T) {
			cfg := &orchestration.Config{Strategy: strategy, Participants: []orchestration.Participant{{AgentName: "worker"}, {AgentName: "manager", Role: "manager"}}, Settings: orchestration.Settings{Aggregator: "synthesis", RouterAgent: "routing", Judge: "judge"}}
			want := []string{"manager", "worker"}
			switch strategy {
			case orchestration.StrategyParallel:
				want = []string{"manager", "synthesis", "worker"}
			case orchestration.StrategyRouter:
				want = []string{"manager", "routing", "worker"}
			case orchestration.StrategyDebate:
				want = []string{"judge", "manager", "worker"}
			}
			got, err := orchestration.ExecutionRoster(cfg)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("roster = %v %v, want %v", got, err, want)
			}
		})
	}
	for _, mode := range []string{"empty", "duplicate", "remote", "blank", "unknown strategy", "unknown route", "missing manager", "concurrency", "rounds", "repair"} {
		t.Run(mode, func(t *testing.T) {
			cfg := &orchestration.Config{Strategy: orchestration.StrategyParallel, Participants: []orchestration.Participant{{AgentName: "worker"}}}
			switch mode {
			case "empty":
				cfg.Participants = nil
			case "duplicate":
				cfg.Participants = append(cfg.Participants, cfg.Participants[0])
			case "remote":
				cfg.Settings.Aggregator = "remote@node"
			case "blank":
				cfg.Participants[0].AgentName = " "
			case "unknown strategy":
				cfg.Strategy = "unknown"
			case "unknown route":
				cfg.Strategy = orchestration.StrategyRouter
				cfg.Settings.RouterRules = map[string]string{"key": "outside"}
			case "missing manager":
				cfg.Strategy = orchestration.StrategyHierarchical
				cfg.Settings.Manager = "outside"
			case "concurrency":
				cfg.Settings.MaxConcurrency = 257
			case "rounds":
				cfg.Settings.Rounds = 129
			case "repair":
				cfg.Strategy = orchestration.StrategyHierarchical
				cfg.Settings.PlanRepairAttempts = 2
			}
			if names, err := orchestration.ExecutionRoster(cfg); err == nil {
				t.Fatalf("invalid roster accepted: %v", names)
			}
		})
	}
}
