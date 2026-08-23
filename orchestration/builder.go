package orchestration

import (
	"errors"
	"fmt"
)

// ErrUnknownStrategy is returned by Build for an unrecognized strategy name.
var ErrUnknownStrategy = errors.New("orchestration: unknown strategy")

// Build constructs the Orchestrator for a strategy name. parts and settings
// come from the Config.
func Build(strategy string, runner AgentRunner, parts []Participant, settings Settings) (Orchestrator, error) {
	switch strategy {
	case StrategySequential:
		return newSequential(runner, parts, settings), nil
	case StrategyParallel:
		return newParallel(runner, parts, settings), nil
	case StrategyRouter:
		return newRouter(runner, parts, settings), nil
	case StrategyHierarchical:
		return newHierarchical(runner, parts, settings), nil
	case StrategyDebate:
		return newDebate(runner, parts, settings), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownStrategy, strategy)
	}
}
