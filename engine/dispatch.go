package engine

import (
	"context"

	"github.com/xraph/cortex/llm"
)

// Dispatch executes a registered or built-in tool by name and returns its raw
// result string. It is the same path the ReAct loop uses (executeTool), exposed
// for hosts and tests that drive tools directly.
func (e *Engine) Dispatch(ctx context.Context, name, arguments string) string {
	return e.executeTool(ctx, llm.ToolCall{Name: name, Arguments: arguments})
}
