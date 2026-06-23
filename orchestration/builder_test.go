package orchestration

import (
	"errors"
	"testing"
)

func TestBuildAllStrategies(t *testing.T) {
	runner := newFakeRunner()
	parts := []Participant{{AgentName: "a"}}
	for _, strategyName := range []string{
		StrategySequential, StrategyParallel, StrategyRouter, StrategyHierarchical, StrategyDebate,
	} {
		o, err := Build(strategyName, runner, "app1", parts, Settings{})
		if err != nil {
			t.Fatalf("Build(%q): %v", strategyName, err)
		}
		if o.Strategy() != strategyName {
			t.Fatalf("Build(%q).Strategy() = %q, want %q", strategyName, o.Strategy(), strategyName)
		}
	}
}

func TestBuildUnknownStrategy(t *testing.T) {
	_, err := Build("nope", newFakeRunner(), "app1", nil, Settings{})
	if !errors.Is(err, ErrUnknownStrategy) {
		t.Fatalf("err = %v, want ErrUnknownStrategy", err)
	}
}
