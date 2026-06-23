// Package orchestration coordinates multiple Cortex agents working together —
// in sequence, in parallel, hierarchically, via a router, or in a debate.
//
// It is a leaf package: it depends only on id and cortex, and reaches
// host capabilities (running an agent, emitting plugin hooks) through injected
// interfaces and callbacks, never by importing the engine or plugin packages.
package orchestration

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// Strategy identifiers. A stored OrchestrationConfig.Strategy is one of these.
const (
	StrategySequential   = "sequential"
	StrategyParallel     = "parallel"
	StrategyRouter       = "router"
	StrategyHierarchical = "hierarchical"
	StrategyDebate       = "debate"
)

// Participant is one agent in an orchestration, with metadata used for awareness:
// the roster of participants is surfaced to each agent so it knows who else is
// taking part and in what role.
type Participant struct {
	AgentName string   `json:"agent_name"`
	Role      string   `json:"role,omitempty"`   // e.g. "manager", "worker", "critic", "judge"
	Skills    []string `json:"skills,omitempty"` // advisory; shown in the roster
}

// Handoff records one agent passing work to another.
type Handoff struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Payload string `json:"payload,omitempty"`
}

// RunOpts is the subset of engine run overrides an orchestrator needs when
// invoking an agent. It is mapped to engine.RunOverrides by the host adapter.
type RunOpts struct {
	Model        string
	Temperature  *float64
	MaxSteps     int
	SystemPrompt string
}

// AgentResult is the strategy-facing view of one completed agent run.
type AgentResult struct {
	AgentName string        `json:"agent_name"`
	RunID     id.AgentRunID `json:"run_id,omitempty"`
	Output    string        `json:"output"`
	Err       error         `json:"-"`
}

// AgentRunner is the single host capability an orchestrator depends on: the
// ability to run one named agent and get its result. The engine satisfies it
// via a thin adapter, avoiding an engine⇄orchestration import cycle.
type AgentRunner interface {
	RunAgent(ctx context.Context, appID, agentName, input string, opts *RunOpts) (*AgentResult, error)
}

// Settings carries every strategy's tunables in one struct. Fields a given
// strategy does not use are ignored.
type Settings struct {
	MaxConcurrency int               `json:"max_concurrency,omitempty"` // parallel / hierarchical worker fan-out cap
	Rounds         int               `json:"rounds,omitempty"`          // debate rounds
	Manager        string            `json:"manager,omitempty"`         // hierarchical: manager agent name
	Judge          string            `json:"judge,omitempty"`           // debate: judge agent name
	Aggregator     string            `json:"aggregator,omitempty"`      // parallel: optional aggregator agent name
	RouterAgent    string            `json:"router_agent,omitempty"`    // router: agent that decides (optional)
	RouterRules    map[string]string `json:"router_rules,omitempty"`    // router: keyword→agent static rules
	Model          string            `json:"model,omitempty"`           // model for meta-decisions (router/judge/manager)
}

// Result is the outcome of an orchestration run.
type Result struct {
	OrchestrationID id.OrchestrationID `json:"orchestration_id"`
	Strategy        string             `json:"strategy"`
	Output          string             `json:"output"`
	AgentResults    []AgentResult      `json:"agent_results,omitempty"`
	Handoffs        []Handoff          `json:"handoffs,omitempty"`
	Elapsed         time.Duration      `json:"elapsed"`
	Err             error              `json:"-"`
}

// Orchestrator is one multi-agent execution strategy. Implementations live in
// Plan 2 (sequential.go, parallel.go, router.go, hierarchical.go, debate.go).
type Orchestrator interface {
	Strategy() string
	Run(ctx context.Context, input string, bb *Blackboard) (*Result, error)
}
