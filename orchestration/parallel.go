package orchestration

import (
	"context"
	"strings"
	"sync"
)

type parallel struct {
	runner   AgentRunner
	appID    string
	parts    []Participant
	settings Settings
}

func newParallel(runner AgentRunner, appID string, parts []Participant, settings Settings) *parallel {
	return &parallel{runner: runner, appID: appID, parts: parts, settings: settings}
}

func (o *parallel) Strategy() string { return StrategyParallel }

func (o *parallel) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategyParallel}
	opts := runOptsFromSettings(o.settings)
	snapshot := bb.Snapshot()

	results := make([]AgentResult, len(o.parts))
	sem := make(chan struct{}, boundedConcurrency(o.settings.MaxConcurrency))
	var wg sync.WaitGroup

	for i, p := range o.parts {
		wg.Add(1)
		go func(i int, p Participant) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ar, err := o.runner.RunAgent(ctx, o.appID, p.AgentName, composeInput(input, snapshot), opts)
			if err != nil {
				results[i] = AgentResult{AgentName: p.AgentName, Err: err}
				return
			}
			results[i] = *ar
		}(i, p)
	}
	wg.Wait()

	var firstErr error
	var parts []string
	for _, r := range results {
		res.AgentResults = append(res.AgentResults, r)
		if r.Err != nil {
			if firstErr == nil {
				firstErr = r.Err
			}
			continue
		}
		bb.Append(r.AgentName, r.Output)
		parts = append(parts, r.Output)
	}

	// Optional aggregator agent synthesizes a single answer from all outputs.
	if o.settings.Aggregator != "" {
		ar, err := o.runner.RunAgent(ctx, o.appID, o.settings.Aggregator, composeInput(input, bb.Snapshot()), opts)
		if err == nil {
			res.AgentResults = append(res.AgentResults, *ar)
			res.Output = ar.Output
			res.Handoffs = bb.Handoffs()
			return res, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}

	res.Output = strings.Join(parts, "\n\n")
	res.Handoffs = bb.Handoffs()
	res.Err = firstErr
	return res, firstErr
}
