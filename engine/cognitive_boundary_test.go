package engine

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/cognitive"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
)

func TestOperationalObservationRejectsUnboundContext(t *testing.T) {
	for _, mode := range []string{"missing_context", "wrong_run", "wrong_agent", "wrong_scope", "arguments", "null", "no_snapshot"} {
		t.Run(mode, func(t *testing.T) {
			e, err := New(WithOperationalAwareness())
			require.NoError(t, err)
			scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "one"}}}
			r := &run.Run{ID: id.NewAgentRunID(), AgentID: id.NewAgentID()}
			cfg := resolvedConfig{Budget: execution.Budget{MaxSteps: 3}}
			st := &reactState{preparedSnapshot: &preparedSnapshot{Scope: scope, Agent: agent.Config{ID: r.AgentID}, Tools: e.toolDefinitions(nil, false)}}
			ctx := withOperationalRuntime(cortex.WithScope(context.Background(), scope), r, cfg, st)
			inv := cortex.Invocation{Subject: cortex.Subject{Scope: scope, RunID: r.ID, AgentID: r.AgentID}, Call: llm.ToolCall{Name: ToolOperationalCapabilities, Arguments: `{}`}}
			switch mode {
			case "missing_context":
				ctx = context.Background()
			case "wrong_run":
				inv.RunID = id.NewAgentRunID()
			case "wrong_agent":
				inv.AgentID = id.NewAgentID()
			case "wrong_scope":
				inv.Scope = cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "two"}}}
			case "arguments":
				inv.Call.Arguments = `{"scope":"two"}`
			case "null":
				inv.Call.Arguments = `null`
			case "no_snapshot":
				st.preparedSnapshot = nil
			}
			content, err := e.operationalCapabilities(ctx, inv)
			require.Error(t, err)
			require.Empty(t, content)
		})
	}
}

func TestOperationalObservationDoesNotInventMissingExecutors(t *testing.T) {
	e, err := New(WithOperationalAwareness())
	require.NoError(t, err)
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "one"}}}
	r := &run.Run{ID: id.NewAgentRunID(), AgentID: id.NewAgentID()}
	cfg := resolvedConfig{Budget: execution.Budget{MaxSteps: 3}}
	defs := append(e.toolDefinitions(nil, false), llm.Tool{Name: "removed"})
	st := &reactState{preparedSnapshot: &preparedSnapshot{Scope: scope, Agent: agent.Config{ID: r.AgentID}, Tools: defs}}
	ctx := withOperationalRuntime(cortex.WithScope(context.Background(), scope), r, cfg, st)
	content, err := e.operationalCapabilities(ctx, cortex.Invocation{Subject: cortex.Subject{Scope: scope, RunID: r.ID, AgentID: r.AgentID}, Call: llm.ToolCall{Name: ToolOperationalCapabilities, Arguments: `{}`}})
	require.NoError(t, err)
	var view cognitive.Snapshot
	require.NoError(t, json.Unmarshal([]byte(content), &view))
	require.Len(t, view.Tools, 2)
	require.Equal(t, "removed", view.Tools[1].Name)
	require.Equal(t, cognitive.CapabilityUnavailable, view.Tools[1].State)
}
