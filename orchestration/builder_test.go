package orchestration

import (
	"errors"
	"testing"
)

func TestBuildAllStrategies(t *testing.T) {
	runner := newFakeRunner()
	parts := []Participant{{AgentName: "a"}}
	for _, strat := range []string{
		StrategySequential, StrategyParallel, StrategyRouter, StrategyHierarchical, StrategyDebate,
	} {
		o, err := Build(strat, runner, "app1", parts, Settings{})
		if err != nil {
			t.Fatalf("Build(%q): %v", strat, err)
		}
		if o.Strategy() != strat {
			t.Fatalf("Build(%q).Strategy() = %q", strat, o.Strategy())
		}
	}
}

func TestBuildUnknownStrategy(t *testing.T) {
	_, err := Build("nope", newFakeRunner(), "app1", nil, Settings{})
	if !errors.Is(err, ErrUnknownStrategy) {
		t.Fatalf("err = %v, want ErrUnknownStrategy", err)
	}
}
