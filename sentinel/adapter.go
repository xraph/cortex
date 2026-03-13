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
	eng   *engine.Engine
	appID string
}

// NewAgentClient creates a Sentinel AgentClient backed by a Cortex engine.
func NewAgentClient(eng *engine.Engine, appID string) target.AgentClient {
	return &cortexAgentClient{eng: eng, appID: appID}
}

func (c *cortexAgentClient) Run(ctx context.Context, agentID, personaRef, input string) (*target.AgentResponse, error) {
	var overrides *engine.RunOverrides
	if personaRef != "" {
		overrides = &engine.RunOverrides{PersonaRef: personaRef}
	}

	r, err := c.eng.RunAgent(ctx, c.appID, agentID, input, overrides)
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
