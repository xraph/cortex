package engine

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
)

// Dispatch executes a registered or built-in tool by name and returns its raw
// result string. It is the same path the ReAct loop uses (executeTool), exposed
// for hosts and tests that drive tools directly. The subject carries only
// whatever scope is on ctx — a direct Dispatch call has no agent or run of
// its own — so an authorizer keyed on those will see them zero-valued.
func (e *Engine) Dispatch(ctx context.Context, name, arguments string) string {
	subject := cortex.Subject{Scope: cortex.ScopeFromContext(ctx)}
	return e.executeTool(ctx, subject, llm.ToolCall{Name: name, Arguments: arguments})
}
