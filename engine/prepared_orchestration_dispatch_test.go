package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
)

func TestResumedTeamChildRechecksAccountBeforeEveryDispatch(t *testing.T) {
	for _, name := range []string{"sync/provider", "sync/tool", "stream/provider", "stream/tool"} {
		t.Run(name, func(t *testing.T) {
			mode, closeAt, _ := strings.Cut(name, "/")
			f := newPreparedTeamFixture(t, orchestration.StrategySequential)
			tools := 0
			refreshPreparedTeam(t, &f, engine.WithLLM(teamDispatchStreamClient{f.client}), engine.WithExternalTool(llm.Tool{Name: "await"}), engine.WithTool(llm.Tool{Name: "probe"}, func(ctx context.Context, _ cortex.Invocation) (string, error) {
				tools++
				if closeAt == "tool" {
					require.NoError(t, f.s.CloseOrchestrationBudget(ctx, f.policy.ExecutionID))
				}
				return "result", nil
			}))
			budgets := make(map[string]execution.Budget)
			for name, limit := range f.policy.Participants {
				limit.Steps = 4
				f.policy.Participants[name] = limit
				budgets[name] = execution.Budget{MaxSteps: 4, MaxTokens: 100, MaxToolCalls: 2, Deadline: f.policy.Deadline}
			}
			var err error
			f.p, err = f.e.PrepareOrchestrationWithBudgets(f.ctx, f.cfg.ID, budgets)
			require.NoError(t, err)
			f.policy.PreparedDigest = f.p.Digest()
			f.client.complete = func(ctx context.Context, _ *llm.Request, call int) (*llm.Response, error) {
				if call == 1 {
					return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "await", Name: "await", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
				}
				if call == 2 {
					if closeAt == "provider" {
						require.NoError(t, f.s.CloseOrchestrationBudget(ctx, f.policy.ExecutionID))
					}
					return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "probe", Name: "probe", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
				}
				return &llm.Response{Content: "should not run", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
			}
			team, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
			require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
			input := engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "await", Content: "ready"}}}
			if mode == "stream" {
				events := make(chan engine.StreamEvent, 32)
				require.NoError(t, f.e.ResumeStream(f.ctx, team.AgentRunIDs[0], input, events))
				var last engine.StreamEvent
				for event := range events {
					last = event
				}
				require.Equal(t, engine.EventError, last.Type)
				child, readErr := f.s.GetRun(f.ctx, team.AgentRunIDs[0])
				require.NoError(t, readErr)
				require.Equal(t, run.StateFailed, child.State)
			} else {
				_, err = f.e.Resume(f.ctx, team.AgentRunIDs[0], input)
				require.Error(t, err)
			}
			require.Equal(t, 2, f.client.calls)
			expectedTools := 0
			if closeAt == "tool" {
				expectedTools = 1
			}
			require.Equal(t, expectedTools, tools)
		})
	}
}

type teamDispatchStreamClient struct{ *preparedTeamClient }

func (c teamDispatchStreamClient) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	response, err := c.Complete(ctx, req)
	if err != nil {
		return nil, err
	}
	return &requiredStream{chunks: []llm.Chunk{{Content: response.Content, ToolCalls: response.ToolCalls, FinishReason: response.FinishReason}}}, nil
}
