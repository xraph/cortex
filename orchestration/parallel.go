package orchestration

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
)

type parallel struct {
	runner   AgentRunner
	parts    []Participant
	settings Settings
}

func newParallel(runner AgentRunner, parts []Participant, settings Settings) *parallel {
	return &parallel{runner: runner, parts: parts, settings: settings}
}

func (o *parallel) Strategy() string { return StrategyParallel }

func (o *parallel) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategyParallel}
	snapshot := bb.Snapshot()

	results := make([]AgentResult, len(o.parts))
	sem := make(chan struct{}, boundedConcurrency(o.settings.MaxConcurrency))
	var wg sync.WaitGroup

	for i, p := range o.parts {
		wg.Add(1)
		go func(i int, p Participant) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				results[i] = AgentResult{AgentName: p.AgentName, Err: ctx.Err()}
				return
			}
			ar, err := invokeAgent(ctx, o.runner, o.settings, p.AgentName, composeInput(input, snapshot), "parallel/worker/"+strconv.Itoa(i))
			ar.Err = err
			results[i] = ar
		}(i, p)
	}
	wg.Wait()

	var workerErrors []error
	var parts []string
	for _, r := range results {
		res.AgentResults = append(res.AgentResults, r)
		if r.Err != nil {
			workerErrors = append(workerErrors, r.Err)
			continue
		}
		bb.Append(r.AgentName, r.Output)
		parts = append(parts, r.Output)
	}
	res.Handoffs = bb.Handoffs()
	if err := errors.Join(workerErrors...); err != nil {
		res.Err = err
		return res, err
	}

	// Synthesis requires a valid result from every worker.
	if o.settings.Aggregator != "" {
		ar, err := invokeAgent(ctx, o.runner, o.settings, o.settings.Aggregator, composeInput(input, bb.Snapshot()), "parallel/aggregate")
		res.AgentResults = append(res.AgentResults, ar)
		res.Err = err
		if err != nil {
			return res, err
		}
		res.Output = ar.Output
	} else {
		res.Output = strings.Join(parts, "\n\n")
	}
	return res, nil
}
