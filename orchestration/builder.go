package orchestration

import (
	"errors"
	"fmt"
)

// ErrUnknownStrategy is returned by Build for an unrecognized strategy name.
var ErrUnknownStrategy = errors.New("orchestration: unknown strategy")

// Build constructs the Orchestrator for a strategy name. appID is the app scope
// passed to every agent run; parts and settings come from the Config.
func Build(strategy string, runner AgentRunner, appID string, parts []Participant, settings Settings) (Orchestrator, error) {
	switch strategy {
	case StrategySequential:
		return newSequential(runner, appID, parts, settings), nil
	case StrategyParallel:
		return newParallel(runner, appID, parts, settings), nil
	case StrategyRouter:
		return newRouter(runner, appID, parts, settings), nil
	case StrategyHierarchical:
		return newHierarchical(runner, appID, parts, settings), nil
	case StrategyDebate:
		return newDebate(runner, appID, parts, settings), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownStrategy, strategy)
	}
}
