package sentinel

import (
	"context"
	"fmt"

	"github.com/xraph/cortex/engine"

	"github.com/xraph/sentinel/evalrun"
	"github.com/xraph/sentinel/target"
)

// cortexAgentClient adapts *engine.Engine to Sentinel's target.AgentClient.
type cortexAgentClient struct {
	eng *engine.Engine
}

// NewAgentClient creates a Sentinel AgentClient backed by a Cortex engine.
// appID is accepted for source compatibility but no longer used: agent
// lookup is scope-guarded now (scope travels on ctx), so app_id doesn't
// participate in resolving the agent to run.
func NewAgentClient(eng *engine.Engine, _ string) target.AgentClient {
	return &cortexAgentClient{eng: eng}
}

func (c *cortexAgentClient) Run(ctx context.Context, agentID, personaRef, input string) (*target.AgentResponse, error) {
	var overrides *engine.RunOverrides
	if personaRef != "" {
		overrides = &engine.RunOverrides{PersonaRef: personaRef}
	}

	r, err := c.eng.RunAgent(ctx, agentID, input, overrides)
	if err != nil {
		return nil, fmt.Errorf("cortex run: %w", err)
	}

	// Fetch steps for the run.
	steps, err := c.eng.ListSteps(ctx, r.ID)
	if err != nil {
		return nil, fmt.Errorf("list steps: %w", err)
	}

	trace := &evalrun.RunTrace{}

	for _, s := range steps {
		trace.Steps = append(trace.Steps, evalrun.StepTrace{
			Index:      s.Index,
			Type:       s.Type,
			Output:     s.Output,
			TokensUsed: s.TokensUsed,
		})

		// Fetch tool calls for each step.
		toolCalls, err := c.eng.ListToolCalls(ctx, s.ID)
		if err != nil {
			return nil, fmt.Errorf("list tool calls for step %d: %w", s.Index, err)
		}
		for _, tc := range toolCalls {
			trace.ToolCalls = append(trace.ToolCalls, evalrun.ToolTrace{
				ToolName:  tc.ToolName,
				Arguments: tc.Arguments,
				Result:    tc.Result,
				Error:     tc.Error,
			})
		}
	}

	return &target.AgentResponse{
		Output: r.Output,
		Tokens: r.TokensUsed,
		Trace:  trace,
	}, nil
}
