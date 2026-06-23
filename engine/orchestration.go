package engine

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

// agentRunnerAdapter adapts the engine's RunAgent to orchestration.AgentRunner.
type agentRunnerAdapter struct{ eng *Engine }

func (a agentRunnerAdapter) RunAgent(ctx context.Context, appID, agentName, input string, opts *orchestration.RunOpts) (*orchestration.AgentResult, error) {
	r, err := a.eng.RunAgent(ctx, appID, agentName, input, mapRunOpts(opts))
	if err != nil {
		return nil, err
	}
	return &orchestration.AgentResult{
		AgentName: agentName,
		RunID:     r.ID,
		Output:    r.Output,
	}, nil
}

func mapRunOpts(o *orchestration.RunOpts) *RunOverrides {
	if o == nil {
		return nil
	}
	return &RunOverrides{
		Model:        o.Model,
		Temperature:  o.Temperature,
		MaxSteps:     o.MaxSteps,
		SystemPrompt: o.SystemPrompt,
	}
}

// registryHookEmitter adapts the plugin Registry to orchestration.HookEmitter.
type registryHookEmitter struct{ eng *Engine }

func (h registryHookEmitter) OrchestrationStarted(ctx context.Context, orchID id.OrchestrationID, strategy string) {
	if h.eng.extensions != nil {
		h.eng.extensions.EmitOrchestrationStarted(ctx, orchID, strategy)
	}
}

func (h registryHookEmitter) OrchestrationCompleted(ctx context.Context, orchID id.OrchestrationID, elapsed time.Duration) {
	if h.eng.extensions != nil {
		h.eng.extensions.EmitOrchestrationCompleted(ctx, orchID, elapsed)
	}
}

func (h registryHookEmitter) AgentHandoff(ctx context.Context, orchID id.OrchestrationID, from, to, payload string) {
	if h.eng.extensions != nil {
		h.eng.extensions.EmitAgentHandoff(ctx, orchID, from, to, payload)
	}
}

// RunOrchestration loads a stored orchestration config by name and executes it,
// persisting a Run and firing orchestration lifecycle hooks.
func (e *Engine) RunOrchestration(ctx context.Context, appID, name, input string) (*orchestration.Run, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	svc := orchestration.NewService(
		agentRunnerAdapter{eng: e},
		e.store,
		e.store,
		registryHookEmitter{eng: e},
	)
	return svc.Run(ctx, appID, name, input)
}
