package engine

import (
	"context"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/suspension"
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
	result, outcome, reason := e.executeTool(ctx, subject, llm.ToolCall{Name: name, Arguments: arguments})
	// A pending call can only be answered by suspending a run, and
	// Dispatch has no run to suspend. Returning executeTool's empty
	// pending result verbatim would hand the caller a blank string that
	// reads like a tool that ran and said nothing. The two reasons get
	// their own sentence, because they send the caller to different
	// places: one needs a result, the other needs a decision.
	if outcome == outcomePending {
		if reason == suspension.ReasonApproval {
			return jsonResult("error", "tool "+name+" requires approval: it is answered by resolving the checkpoint on a suspended run, not through Dispatch")
		}
		return jsonResult("error", "tool "+name+" is external: it is executed by the caller when a run suspends, not through Dispatch")
	}
	return result
}
