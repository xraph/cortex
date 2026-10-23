package engine

import (
	"context"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
	"github.com/xraph/cortex/suspension"
)

type cancellingStepHook struct{ cancel context.CancelFunc }

func (*cancellingStepHook) Name() string { return "cancel-before-dispatch" }
func (h *cancellingStepHook) OnStepStarted(context.Context, id.AgentRunID, int) error {
	h.cancel()
	return nil
}

type cancellationClient struct {
	outcomeClient
	calls int
}

func (c *cancellationClient) Complete(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	c.calls++
	return c.outcomeClient.Complete(ctx, req)
}

func TestSyncCancellationDuringStepHookPreventsDispatch(t *testing.T) {
	ctx, cancel := context.WithCancel(cortex.WithScope(context.Background(), scopeA()))
	defer cancel()
	client := &cancellationClient{outcomeClient: outcomeClient{response: &llm.Response{Content: "done", FinishReason: "stop"}}}
	e, err := New(WithStore(scopespy.New()), WithLLM(client), WithExtension(&cancellingStepHook{cancel: cancel}))
	if err != nil {
		t.Fatal(err)
	}
	result, _ := e.RunAgent(ctx, "assistant", "work", nil)
	if client.calls != 0 {
		t.Fatalf("dispatched %d provider requests after cancellation", client.calls)
	}
	if result != nil && result.State == run.StateCompleted {
		t.Fatal("cancelled run completed")
	}
}

type cancellingAuthorizer struct{ cancel context.CancelFunc }

func (*cancellingAuthorizer) Visible(_ context.Context, _ cortex.Subject, defs []llm.Tool) []llm.Tool {
	return defs
}
func (a *cancellingAuthorizer) Authorize(context.Context, cortex.Subject, llm.ToolCall) error {
	a.cancel()
	return nil
}

func TestCancellationDuringAuthorizationPreventsToolDispatch(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal", true: "resumed"}[resumed], func(t *testing.T) {
			ctx, cancel := context.WithCancel(cortex.WithScope(context.Background(), scopeA()))
			defer cancel()
			calls := 0
			e, err := New(WithToolAuthorizer(&cancellingAuthorizer{cancel: cancel}), WithTool(llm.Tool{Name: "effect"}, func(context.Context, cortex.Invocation) (string, error) { calls++; return "effect happened", nil }))
			if err != nil {
				t.Fatal(err)
			}
			subject := cortex.Subject{Scope: scopeA(), AgentID: id.NewAgentID(), RunID: id.NewAgentRunID()}
			if resumed {
				_, errText, _ := e.resumedCallResult(ctx, subject, suspension.PendingCall{ID: "call", Name: "effect"}, ToolResult{Execute: true}, true)
				if errText == "" {
					t.Error("cancelled tool reported success")
				}
			} else {
				_, outcome, _ := e.executeTool(ctx, subject, llm.ToolCall{ID: "call", Name: "effect"})
				if outcome == outcomeCompleted {
					t.Error("cancelled tool reported success")
				}
			}
			if calls != 0 {
				t.Fatalf("dispatched %d effects after authorization cancelled", calls)
			}
		})
	}
}
