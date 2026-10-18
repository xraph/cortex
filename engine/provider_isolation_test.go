package engine

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
)

type temperatureMutatingClient struct{ temperatures []float64 }

func (c *temperatureMutatingClient) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	c.temperatures = append(c.temperatures, *req.Temperature)
	*req.Temperature = 1.9
	if len(c.temperatures) == 1 {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "call", Name: "probe", Arguments: `{}`}}, FinishReason: "tool_calls"}, nil
	}
	return &llm.Response{Content: "done", FinishReason: "stop"}, nil
}

func (c *temperatureMutatingClient) CompleteStream(ctx context.Context, req *llm.Request) (llm.Stream, error) {
	response, err := c.Complete(ctx, req)
	return &outcomeStream{response: response}, err
}

func TestProviderCannotMutateFrozenTemperature(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		t.Run(map[bool]string{false: "sync", true: "stream"}[streaming], func(t *testing.T) {
			client := &temperatureMutatingClient{}
			s := scopespy.New()
			e, err := New(WithStore(s), WithLLM(client), WithTool(llm.Tool{Name: "probe"}, func(context.Context, cortex.Invocation) (string, error) { return "ok", nil }))
			if err != nil {
				t.Fatal(err)
			}
			temperature := 0.2
			ctx := cortex.WithScope(context.Background(), scopeA())
			if streaming {
				events := make(chan StreamEvent, 32)
				if err = e.StreamAgent(ctx, "assistant", "work", &RunOverrides{Temperature: &temperature}, events); err != nil {
					t.Fatal(err)
				}
				for event := range events {
					if event.Type == EventError {
						t.Fatalf("stream error: %v", event.Data)
					}
				}
			} else if _, err = e.RunAgent(ctx, "assistant", "work", &RunOverrides{Temperature: &temperature}); err != nil {
				t.Fatal(err)
			}
			if len(client.temperatures) != 2 || client.temperatures[1] != temperature {
				t.Fatalf("provider changed later configuration: %v", client.temperatures)
			}
			if runs := s.Runs(); len(runs) != 1 || runs[0].State != run.StateCompleted {
				t.Fatal("run did not complete")
			}
		})
	}
}
