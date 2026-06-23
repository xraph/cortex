package orchestration

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
)

type hierarchical struct {
	runner   AgentRunner
	appID    string
	parts    []Participant
	settings Settings
}

func newHierarchical(runner AgentRunner, appID string, parts []Participant, settings Settings) *hierarchical {
	return &hierarchical{runner: runner, appID: appID, parts: parts, settings: settings}
}

func (o *hierarchical) Strategy() string { return StrategyHierarchical }

type delegation struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

func (o *hierarchical) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategyHierarchical}
	opts := runOptsFromSettings(o.settings)

	manager := o.resolveManager()
	if manager == "" {
		return res, nil
	}
	workers := nonManagerParticipants(o.parts, manager)

	// 1. Manager produces a delegation plan.
	planOut, err := o.runner.RunAgent(ctx, o.appID, manager, buildPlanPrompt(workers, input), opts)
	if err != nil {
		res.Err = err
		return res, err
	}
	res.AgentResults = append(res.AgentResults, *planOut)
	bb.Append(manager, planOut.Output)

	tasks := parsePlan(planOut.Output, workers, input)

	// 2. Workers execute their tasks (bounded concurrency).
	workerResults := make([]AgentResult, len(tasks))
	sem := make(chan struct{}, boundedConcurrency(o.settings.MaxConcurrency))
	var wg sync.WaitGroup
	for i, d := range tasks {
		wg.Add(1)
		go func(i int, d delegation) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ar, werr := o.runner.RunAgent(ctx, o.appID, d.Agent, composeInput(d.Task, bb.Snapshot()), opts)
			if werr != nil {
				workerResults[i] = AgentResult{AgentName: d.Agent, Err: werr}
				return
			}
			workerResults[i] = *ar
		}(i, d)
	}
	wg.Wait()

	for i, r := range workerResults {
		res.AgentResults = append(res.AgentResults, r)
		if r.Err != nil {
			continue
		}
		bb.Append(r.AgentName, r.Output)
		bb.Handoff(ctx, manager, tasks[i].Agent, tasks[i].Task)
	}

	// 3. Manager synthesizes the final answer from the workers' contributions.
	synth, err := o.runner.RunAgent(ctx, o.appID, manager, buildSynthesisPrompt(input, bb.Snapshot()), opts)
	if err != nil {
		res.Err = err
		res.Handoffs = bb.Handoffs()
		return res, err
	}
	res.AgentResults = append(res.AgentResults, *synth)
	res.Output = synth.Output
	res.Handoffs = bb.Handoffs()
	return res, nil
}

func (o *hierarchical) resolveManager() string {
	if o.settings.Manager != "" {
		return o.settings.Manager
	}
	for _, p := range o.parts {
		if strings.EqualFold(p.Role, "manager") {
			return p.AgentName
		}
	}
	if len(o.parts) > 0 {
		return o.parts[0].AgentName
	}
	return ""
}

// parsePlan extracts a delegation list from the manager output, keeping only
// tasks addressed to known workers. Falls back to assigning the original input
// to every worker when the plan is missing or unparseable.
func parsePlan(output string, workers []Participant, input string) []delegation {
	var plan []delegation
	if start := strings.Index(output, "["); start >= 0 {
		if end := strings.LastIndex(output, "]"); end > start {
			_ = json.Unmarshal([]byte(output[start:end+1]), &plan)
		}
	}
	var valid []delegation
	for _, d := range plan {
		if d.Agent == "" || d.Task == "" {
			continue
		}
		if _, ok := findParticipant(workers, d.Agent); ok {
			valid = append(valid, d)
		}
	}
	if len(valid) > 0 {
		return valid
	}
	fallback := make([]delegation, len(workers))
	for i, w := range workers {
		fallback[i] = delegation{Agent: w.AgentName, Task: input}
	}
	return fallback
}

func buildPlanPrompt(workers []Participant, input string) string {
	var sb strings.Builder
	sb.WriteString("You are a manager. Break the task into subtasks for your team.\n")
	sb.WriteString(`Respond ONLY with a JSON array like [{"agent":"name","task":"..."}].` + "\n\nTeam:\n")
	for _, w := range workers {
		if w.Role != "" {
			sb.WriteString("- " + w.AgentName + " (" + w.Role + ")\n")
		} else {
			sb.WriteString("- " + w.AgentName + "\n")
		}
	}
	sb.WriteString("\nTask: " + input)
	return sb.String()
}

func buildSynthesisPrompt(input, snapshot string) string {
	return snapshot + "\n\nUsing your team's work above, produce the final answer to: " + input
}
