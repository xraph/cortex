package orchestration

import "context"

type sequential struct {
	runner   AgentRunner
	appID    string
	parts    []Participant
	settings Settings
}

func newSequential(runner AgentRunner, appID string, parts []Participant, settings Settings) *sequential {
	return &sequential{runner: runner, appID: appID, parts: parts, settings: settings}
}

func (o *sequential) Strategy() string { return StrategySequential }

func (o *sequential) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategySequential}
	opts := runOptsFromSettings(o.settings)

	last := input
	for i, p := range o.parts {
		agentInput := composeInput(input, bb.Snapshot())
		ar, err := o.runner.RunAgent(ctx, o.appID, p.AgentName, agentInput, opts)
		if err != nil {
			res.AgentResults = append(res.AgentResults, AgentResult{AgentName: p.AgentName, Err: err})
			res.Err = err
			return res, err
		}
		bb.Append(p.AgentName, ar.Output)
		if i > 0 {
			bb.Handoff(ctx, o.parts[i-1].AgentName, p.AgentName, last)
		}
		res.AgentResults = append(res.AgentResults, *ar)
		last = ar.Output
	}
	res.Output = last
	res.Handoffs = bb.Handoffs()
	return res, nil
}
