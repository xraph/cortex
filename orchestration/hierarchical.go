package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/xraph/cortex"
)

type hierarchical struct {
	runner   AgentRunner
	parts    []Participant
	settings Settings
}

func newHierarchical(runner AgentRunner, parts []Participant, settings Settings) *hierarchical {
	return &hierarchical{runner: runner, parts: append([]Participant(nil), parts...), settings: settings}
}

func (o *hierarchical) Strategy() string { return StrategyHierarchical }

type delegation struct {
	Agent string `json:"agent"`
	Task  string `json:"task"`
}

func (o *hierarchical) Run(ctx context.Context, input string, bb *Blackboard) (*Result, error) {
	res := &Result{OrchestrationID: bb.OrchestrationID(), Strategy: StrategyHierarchical}
	fail := func(err error) (*Result, error) {
		res.Err = err
		res.Handoffs = bb.Handoffs()
		return res, err
	}
	manager := o.resolveManager()
	workers := nonManagerParticipants(o.parts, manager)
	if err := o.validateRoster(manager, workers); err != nil {
		return fail(err)
	}
	prompt := buildPlanPrompt(workers, input)
	var tasks []delegation
	for attempt := 0; ; attempt++ {
		planOut, err := o.invoke(ctx, manager, prompt)
		res.AgentResults = append(res.AgentResults, planOut)
		if err != nil {
			return fail(err)
		}
		tasks, err = parseDelegations(planOut.Output, workers)
		if err == nil {
			bb.Append(manager, planOut.Output)
			break
		}
		if attempt >= o.settings.PlanRepairAttempts {
			return fail(err)
		}
		// Only bounded validation diagnostics enter the repair prompt. The rejected
		// candidate is retained in its run record, never copied into worker context.
		repair, repairErr := json.Marshal(map[string]string{"validation_error": err.Error()})
		if repairErr != nil {
			return fail(repairErr)
		}
		prompt = buildPlanPrompt(workers, input) + "\nThe previous plan was rejected. Correct it using this validation data: " + string(repair)
	}
	workerResults := make([]AgentResult, len(tasks))
	sem := make(chan struct{}, min(boundedConcurrency(o.settings.MaxConcurrency), len(tasks)))
	snapshot := bb.Snapshot()
	var wg sync.WaitGroup
	for i, d := range tasks {
		wg.Add(1)
		go func(i int, d delegation) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				workerResults[i] = AgentResult{AgentName: d.Agent, Err: ctx.Err()}
				return
			}
			if err := ctx.Err(); err != nil {
				workerResults[i] = AgentResult{AgentName: d.Agent, Err: err}
				return
			}
			bb.Handoff(ctx, manager, d.Agent, d.Task)
			workerResult, workerErr := o.invoke(ctx, d.Agent, composeInput(d.Task, snapshot))
			if workerErr != nil {
				workerResult.Err = workerErr
			}
			workerResults[i] = workerResult
		}(i, d)
	}
	wg.Wait()
	var workerErrors []error
	for _, r := range workerResults {
		res.AgentResults = append(res.AgentResults, r)
		if r.Err != nil {
			workerErrors = append(workerErrors, r.Err)
		} else {
			bb.Append(r.AgentName, r.Output)
		}
	}
	if err := errors.Join(workerErrors...); err != nil {
		return fail(err)
	}
	synth, err := o.invoke(ctx, manager, buildSynthesisPrompt(input, bb.Snapshot()))
	res.AgentResults = append(res.AgentResults, synth)
	if err != nil {
		return fail(err)
	}
	res.Output = synth.Output
	res.Handoffs = bb.Handoffs()
	return res, nil
}

func (o *hierarchical) validateRoster(manager string, workers []Participant) error {
	if o.settings.PlanRepairAttempts < 0 || o.settings.PlanRepairAttempts > 1 {
		return invalidPlan("repair attempts must be zero or one")
	}
	if manager == "" || !findParticipant(o.parts, manager) || len(workers) == 0 || len(workers) > maxDelegationWorkers {
		return invalidPlan("invalid manager or worker count")
	}
	seen := make(map[string]bool, len(o.parts))
	for _, p := range o.parts {
		if strings.TrimSpace(p.AgentName) == "" || seen[p.AgentName] {
			return invalidPlan("invalid participant roster")
		}
		seen[p.AgentName] = true
	}
	return nil
}

// invoke preserves failed run identities and treats missing or contradictory
// results as failures. Each invocation gets its own options and normal budget.
func (o *hierarchical) invoke(ctx context.Context, name, input string) (AgentResult, error) {
	out := AgentResult{AgentName: name}
	if err := ctx.Err(); err != nil {
		out.Err = err
		return out, err
	}
	ar, err := o.runner.RunAgent(ctx, name, input, runOptsFromSettings(o.settings))
	if ar != nil {
		out = *ar
		err = errors.Join(err, ar.Err)
	}
	if err == nil {
		err = ctx.Err()
	}
	if err == nil && (ar == nil || ar.AgentName != name || strings.TrimSpace(ar.Output) == "") {
		err = fmt.Errorf("%w: missing or inconsistent agent result", cortex.ErrRunIncomplete)
	}
	out.Err = err
	return out, err
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

func buildPlanPrompt(workers []Participant, input string) string {
	var sb strings.Builder
	sb.WriteString("You are a manager. Break the task into subtasks for your team. Assign every listed worker exactly once; all workers are required.\n")
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
