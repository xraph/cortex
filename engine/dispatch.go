package engine

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
)

// Dispatch executes a registered or built-in tool by name and returns its raw
// result string. It is the same path the ReAct loop uses (executeTool), exposed
// for hosts and tests that drive tools directly. The subject carries whatever
// scope and principal are on ctx — a direct Dispatch call has no agent or run
// of its own, so an authorizer keyed on those will see them zero-valued.
func (e *Engine) Dispatch(ctx context.Context, name, arguments string) string {
	subject := cortex.Subject{Scope: cortex.ScopeFromContext(ctx), Principal: cortex.PrincipalFromContext(ctx)}
	// The outcome is what tells the ReAct loop whether a ToolCompleted event
	// is still owed. Dispatch has no run to attribute one to, so it drops it.
	result, outcome := e.executeTool(ctx, subject, llm.ToolCall{Name: name, Arguments: arguments})
	// An external tool can only be answered by suspending a run, and
	// Dispatch has no run to suspend. Returning executeTool's empty
	// pending result verbatim would hand the caller a blank string that
	// reads like a tool that ran and said nothing.
	if outcome == outcomePending {
		return jsonResult("error", "tool "+name+" is external: it is executed by the caller when a run suspends, not through Dispatch")
	}
	return result
}
