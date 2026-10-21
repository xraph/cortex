package orchestration

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
)

// PlanRequest supplies host-authored task templates with stable IDs and empty
// dependency lists. Hierarchical input must have a matching captured source in
// Template.Sources and in every worker's source requirements. The host retains
// those source bytes in its authorized evidence repository before adaptation.
type PlanRequest struct {
	Template         planning.Plan
	Limits           planning.Limits
	DelegationPlan   string
	DelegationSource string
}

// AdaptPlan maps compatible legacy strategies to immutable task definitions.
// It never runs an agent, grants a tool, claims work or accepts a task result.
// Router and debate remain available through Build; they have no static mapping.
func AdaptPlan(cfg Config, req PlanRequest) (planning.Plan, error) {
	switch cfg.Strategy {
	case StrategySequential, StrategyParallel, StrategyHierarchical:
	default:
		return planning.Plan{}, fmt.Errorf("%w: %q", ErrUnknownStrategy, cfg.Strategy)
	}
	digest, err := ExecutionConfigDigest(cfg)
	if err != nil {
		return planning.Plan{}, err
	}
	p := req.Template
	if p.Scope.Canonical() != cfg.Scope.Canonical() || p.Revision != 1 || p.Origin != (planning.Origin{}) {
		return planning.Plan{}, adapterError("template identity")
	}
	p, err = planning.Seal(p, req.Limits)
	if err != nil {
		return planning.Plan{}, err
	}
	tasks := make(map[string]planning.Task, len(p.Tasks))
	for _, task := range p.Tasks {
		if _, duplicate := tasks[task.Owner]; duplicate || len(task.DependsOn) != 0 {
			return planning.Plan{}, adapterError("ambiguous task template or existing dependencies")
		}
		tasks[task.Owner] = task
	}
	var graph []planning.Task
	switch cfg.Strategy {
	case StrategySequential, StrategyParallel:
		if req.DelegationPlan != "" || req.DelegationSource != "" {
			return planning.Plan{}, adapterError("unexpected delegation input")
		}
		for _, part := range cfg.Participants {
			task, ok := tasks[part.AgentName]
			if !ok {
				return planning.Plan{}, adapterError("missing participant template")
			}
			if cfg.Strategy == StrategySequential && len(graph) > 0 {
				task.DependsOn = []id.ID{graph[len(graph)-1].ID}
			}
			graph = append(graph, task)
		}
		if cfg.Strategy == StrategyParallel && cfg.Settings.Aggregator != "" {
			if findParticipant(cfg.Participants, cfg.Settings.Aggregator) {
				return planning.Plan{}, adapterError("aggregator also appears as worker")
			}
			aggregator, ok := tasks[cfg.Settings.Aggregator]
			if !ok {
				return planning.Plan{}, adapterError("missing aggregator template")
			}
			aggregator.DependsOn = taskIDs(graph)
			graph = append(graph, aggregator)
		}
	case StrategyHierarchical:
		graph, err = hierarchicalGraph(cfg, req, p, tasks)
		if err != nil {
			return planning.Plan{}, err
		}
	}
	if len(graph) != len(tasks) {
		return planning.Plan{}, adapterError("unused task template")
	}
	p.Tasks = graph
	p.Origin = planning.Origin{Kind: "orchestration", ID: cfg.ID, Digest: digest}
	return planning.Seal(p, req.Limits)
}

func hierarchicalGraph(cfg Config, req PlanRequest, p planning.Plan, tasks map[string]planning.Task) ([]planning.Task, error) {
	hierarchy := hierarchical{parts: cfg.Participants, settings: cfg.Settings}
	manager := hierarchy.resolveManager()
	workers := nonManagerParticipants(cfg.Participants, manager)
	if err := hierarchy.validateRoster(manager, workers); err != nil {
		return nil, err
	}
	assignments, err := parseDelegations(req.DelegationPlan, workers)
	if err != nil {
		return nil, err
	}
	captured := false
	for _, source := range p.Sources {
		if source.Resource == req.DelegationSource && source.Digest == evidence.Digest([]byte(req.DelegationPlan)) {
			captured = true
			break
		}
	}
	if !captured {
		return nil, adapterError("delegation source is not captured")
	}
	instructions := make(map[string]string, len(assignments))
	for _, assignment := range assignments {
		instructions[assignment.Agent] = assignment.Task
	}
	graph := make([]planning.Task, 0, len(tasks))
	for _, worker := range workers {
		task, ok := tasks[worker.AgentName]
		if !ok || !slices.Contains(task.Sources, req.DelegationSource) {
			return nil, adapterError("worker lacks delegation source")
		}
		task.Instruction = instructions[worker.AgentName]
		graph = append(graph, task)
	}
	synthesis, ok := tasks[manager]
	if !ok {
		return nil, adapterError("missing manager template")
	}
	synthesis.DependsOn = taskIDs(graph)
	return append(graph, synthesis), nil
}

func taskIDs(tasks []planning.Task) []id.ID {
	ids := make([]id.ID, len(tasks))
	for i, task := range tasks {
		ids[i] = task.ID
	}
	return ids
}

// ExecutionConfigDigest pins the strategy, participants, settings and scope.
// Display metadata and timestamps are excluded. A future dispatcher must resolve
// this exact configuration or explicitly replan before using changed settings.
func ExecutionConfigDigest(cfg Config) (string, error) {
	if cfg.ID.IsNil() || cfg.ID.Prefix() != id.PrefixOrchestrationConfig || cortex.ValidateScope(cfg.Scope) != nil || len(cfg.Participants) == 0 || len(cfg.Participants) > 256 {
		return "", adapterError("configuration identity or roster")
	}
	if !configText(cfg.Scope.Canonical(), cfg.Strategy, cfg.Settings.Model, cfg.Settings.Manager, cfg.Settings.Judge, cfg.Settings.Aggregator, cfg.Settings.RouterAgent) {
		return "", adapterError("configuration encoding")
	}
	seen := make(map[string]bool, len(cfg.Participants))
	for _, part := range cfg.Participants {
		if strings.TrimSpace(part.AgentName) == "" || seen[part.AgentName] || !configText(part.AgentName, part.Role) || len(part.Skills) > 256 || !configText(part.Skills...) {
			return "", adapterError("participant encoding or identity")
		}
		seen[part.AgentName] = true
	}
	if len(cfg.Settings.RouterRules) > 256 {
		return "", adapterError("routing rule count")
	}
	for key, value := range cfg.Settings.RouterRules {
		if !configText(key, value) {
			return "", adapterError("routing rule encoding")
		}
	}
	snapshot := struct {
		ID           id.ID         `json:"id"`
		Scope        cortex.Scope  `json:"scope"`
		Strategy     string        `json:"strategy"`
		Participants []Participant `json:"participants"`
		Settings     Settings      `json:"settings"`
	}{cfg.ID, cfg.Scope, cfg.Strategy, cfg.Participants, cfg.Settings}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return "", fmt.Errorf("%w: %w", planning.ErrInvalid, err)
	}
	if len(data) > 1<<20 {
		return "", adapterError("configuration exceeds one MiB")
	}
	return evidence.Digest(data), nil
}

func configText(values ...string) bool {
	for _, value := range values {
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return false
		}
	}
	return true
}

func adapterError(reason string) error { return fmt.Errorf("%w: %s", planning.ErrInvalid, reason) }
