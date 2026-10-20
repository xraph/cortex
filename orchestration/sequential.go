package orchestration

import "context"

type sequential struct {
	runner   AgentRunner
	parts    []Participant
	settings Settings
}

func newSequential(runner AgentRunner, parts []Participant, settings Settings) *sequential {
	return &sequential{runner: runner, parts: parts, settings: settings}
}

func (o *sequential) Strategy() string { return StrategySequential }

func (o *sequential) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategySequential}

	last := input
	for i, p := range o.parts {
		agentInput := composeInput(input, bb.Snapshot())
		ar, err := invokeAgent(ctx, o.runner, o.settings, p.AgentName, agentInput)
		res.AgentResults = append(res.AgentResults, ar)
		if err != nil {
			res.Err = err
			return res, err
		}
		bb.Append(p.AgentName, ar.Output)
		if i > 0 {
			bb.Handoff(ctx, o.parts[i-1].AgentName, p.AgentName, last)
		}
		last = ar.Output
	}
	res.Output = last
	res.Handoffs = bb.Handoffs()
	return res, nil
}
