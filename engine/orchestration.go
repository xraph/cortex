package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/telemetry"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
)

// agentRunnerAdapter adapts the engine's RunAgent to orchestration.AgentRunner.
type agentRunnerAdapter struct{ eng *Engine }

func (a agentRunnerAdapter) RunAgent(ctx context.Context, agentName, input string, opts *orchestration.RunOpts) (*orchestration.AgentResult, error) {
	overrides := mapRunOpts(opts)
	if route, rest, ok := a2a.ReplyRouteFrom(ctx); ok {
		// The bus is starting this run for a directive. The route belongs
		// to this run alone, not to runs its tools start.
		ctx = rest
		if overrides == nil {
			overrides = &RunOverrides{}
		}
		overrides.replyRoute = &route
	}
	r, err := a.eng.RunAgent(ctx, agentName, input, overrides)
	return orchestrationAgentResult(agentName, r, err)
}

func orchestrationAgentResult(agentName string, r *run.Run, err error) (*orchestration.AgentResult, error) {
	if r == nil {
		if err == nil {
			err = cortex.ErrRunIncomplete
		}
		return nil, err
	}
	// Only replace the runtime's standalone verification result. A joined error
	// may also contain a budget or dispatch failure and must keep that failure.
	if verificationErr := requiredChildResultError(r); verificationErr != nil && (err == nil || err == cortex.ErrVerificationUnavailable || err == cortex.ErrRunUnverified) { //nolint:errorlint // Exact sentinels only: errors.Is would hide other failures in a joined error.
		err = verificationErr
	}
	if err == nil {
		switch r.State {
		case run.StateCompleted:
		case run.StateCancelled:
			err = cortex.ErrRunCancelled
		default:
			err = fmt.Errorf("cortex: worker %s stopped with %s: %w", agentName, r.StopReason, cortex.ErrRunIncomplete)
		}
	}
	output := r.Output
	if err != nil && (r.Provenance.VerificationRequired || r.Provenance.VerificationIntentID != "") {
		output = ""
	}
	return &orchestration.AgentResult{
		AgentName: agentName,
		RunID:     r.ID,
		Output:    output,
		Err:       err,
	}, err
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
//
// It runs under a run orchestration span, and every participant's run
// agent span nests under it, parallel workers included.
func (e *Engine) RunOrchestration(ctx context.Context, name, input string) (*orchestration.Run, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	svc := orchestration.NewService(
		agentRunnerAdapter{eng: e},
		e.store,
		e.store,
		registryHookEmitter{eng: e},
	)
	ctx, span := telemetry.Tracer().Start(ctx, spanRunOrchestration)
	rec, err := svc.Run(ctx, name, input)
	e.finishOrchestration(span, rec, err)
	return rec, err
}
