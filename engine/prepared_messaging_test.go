package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/sqlite"
)

func allowPreparedMessaging() engine.PreparedTeamMessagingConfig {
	return engine.PreparedTeamMessagingConfig{
		AuthorizeTeam: func(context.Context, id.OrchestrationID) error { return nil },
		AuthorizeRun:  func(context.Context, id.OrchestrationID, id.AgentRunID) error { return nil },
	}
}

func TestPreparedTeamPrivateAskContinuesWithinCoordinator(t *testing.T) {
	for _, strategy := range []string{orchestration.StrategySequential, orchestration.StrategyParallel} {
		t.Run(strategy, func(t *testing.T) { testPreparedTeamPrivateAsk(t, strategy) })
	}
}

func testPreparedTeamPrivateAsk(t *testing.T, strategy string) {
	f := newPreparedTeamFixture(t, strategy)
	refreshPreparedTeam(t, &f, engine.WithA2A(a2a.Options{}), engine.WithPreparedTeamMessaging(allowPreparedMessaging()))
	aCalls := 0
	f.client.complete = func(_ context.Context, req *llm.Request, _ int) (*llm.Response, error) {
		if req.System == "frozen a" {
			aCalls++
			if aCalls == 1 {
				return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "ask-b", Name: "agent_ask", Arguments: `{"to":"b","content":"check the evidence"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
			}
			found := false
			for _, message := range req.Messages {
				if message.Role == "tool" && strings.Contains(message.Content, "worker evidence") {
					found = true
				}
			}
			require.True(t, found, "the original ask must receive the retained private answer")
			return &llm.Response{Content: "team answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
		}
		return &llm.Response{Content: "worker evidence", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	// Mutable names must not change the sender or receiver captured by the team.
	live, err := f.s.GetByName(f.ctx, "a")
	require.NoError(t, err)
	live.Name = "renamed-a"
	require.NoError(t, f.s.Update(f.ctx, live))
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	parent, err := f.e.RunPreparedOrchestration(ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusCompleted, parent.Status)
	children, calls := 3, 4
	if strategy == orchestration.StrategyParallel {
		children, calls = 4, 5
	}
	require.Len(t, parent.AgentRunIDs, children)
	require.Equal(t, calls, f.client.calls)
	global, err := f.s.ListMessages(ctx, nil)
	require.NoError(t, err)
	require.Empty(t, global)
	private, err := a2a.NewRoutingStore(f.s, parent.ID)
	require.NoError(t, err)
	messages, err := private.ListMessages(ctx, nil)
	require.NoError(t, err)
	require.Len(t, messages, 2)
	for _, message := range messages {
		require.Equal(t, parent.ID.String(), message.OrchestrationID)
		require.NotEqual(t, "renamed-a", message.Sender.Agent)
	}
	budget, err := f.s.GetOrchestrationBudgetSnapshot(ctx, parent.ID)
	require.NoError(t, err)
	require.EqualValues(t, children, budget.Charged.FanOut)
}

func TestPreparedTeamPrivateAskRecoversAfterRecipientResume(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	permitted := true
	config := allowPreparedMessaging()
	denied := errors.New("team permission revoked")
	config.AuthorizeTeam = func(context.Context, id.OrchestrationID) error {
		if !permitted {
			return denied
		}
		return nil
	}
	opts := []engine.Option{engine.WithA2A(a2a.Options{}), engine.WithPreparedTeamMessaging(config), engine.WithExternalTool(llm.Tool{Name: "await"})}
	refreshPreparedTeam(t, &f, opts...)
	aCalls, bCalls := 0, 0
	f.client.complete = func(_ context.Context, req *llm.Request, _ int) (*llm.Response, error) {
		if req.System == "frozen a" {
			aCalls++
			if aCalls == 1 {
				return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "ask-b", Name: "agent_ask", Arguments: `{"to":"b","content":"check"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
			}
		} else {
			bCalls++
			if bCalls == 1 {
				return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "external", Name: "await", Arguments: `{}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
			}
		}
		return &llm.Response{Content: "retained answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	parent, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
	require.Equal(t, orchestration.StatusWaiting, parent.Status)
	require.Len(t, parent.AgentRunIDs, 2)
	original := append([]id.AgentRunID(nil), parent.AgentRunIDs...)
	var recipient id.AgentRunID
	for _, key := range parent.AgentRunIDs {
		child, readErr := f.s.GetRun(f.ctx, key)
		require.NoError(t, readErr)
		require.Equal(t, run.StatePaused, child.State)
		pause, readErr := f.s.GetSuspension(f.ctx, key)
		require.NoError(t, readErr)
		if pause.Pending[0].Name == "await" {
			recipient = key
		}
	}
	require.False(t, recipient.IsNil())
	opts = append([]engine.Option{engine.WithStore(f.s), engine.WithLLM(f.client)}, opts...)
	fresh, err := engine.New(opts...)
	require.NoError(t, err)
	permitted = false
	_, err = fresh.Resume(f.ctx, recipient, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "external", Content: "done"}}})
	require.ErrorIs(t, err, denied)
	require.Equal(t, 2, f.client.calls)
	permitted = true
	_, err = fresh.Resume(f.ctx, recipient, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "external", Content: "done"}}})
	require.NoError(t, err)
	// A host run grant alone cannot replace the private team's coordinator lease.
	for _, key := range original {
		if key == recipient {
			continue
		}
		paused, readErr := f.s.GetSuspension(f.ctx, key)
		require.NoError(t, readErr)
		_, recoverErr := fresh.RecoverAgentReply(f.ctx, key, paused.ID, func(context.Context, id.AgentRunID) error { return nil })
		require.ErrorIs(t, recoverErr, cortex.ErrOrchestrationContinuationUnavailable)
	}
	require.Equal(t, 3, f.client.calls)
	source, err := coordination.NewScopedOrchestrationSource(f.s, []cortex.Scope{f.policy.Scope}, func(context.Context, cortex.Scope) error { return nil })
	require.NoError(t, err)
	require.NoError(t, engine.WithOrchestrationRecovery(engine.OrchestrationRecoveryConfig{Service: coordination.OrchestrationRecoveryConfig{Discovery: source, Interval: 10 * time.Millisecond}, Authorize: func(context.Context, id.OrchestrationID) error { return nil }})(fresh))
	require.NoError(t, fresh.Start(f.ctx))
	t.Cleanup(func() { require.NoError(t, fresh.Stop(context.Background())) })
	require.Eventually(t, func() bool {
		current, readErr := f.s.GetOrchestrationRun(f.ctx, parent.ID)
		return readErr == nil && current.Status == orchestration.StatusCompleted
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, fresh.Stop(context.Background()))
	completed, err := f.s.GetOrchestrationRun(f.ctx, parent.ID)
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusCompleted, completed.Status)
	require.Len(t, completed.AgentRunIDs, 3)
	for _, key := range original {
		require.Contains(t, completed.AgentRunIDs, key)
	}
	require.Equal(t, 5, f.client.calls)
}

func TestPreparedTeamMessagingConfigurationFailsClosed(t *testing.T) {
	_, err := engine.New(engine.WithPreparedTeamMessaging(engine.PreparedTeamMessagingConfig{}))
	require.Error(t, err)
	_, err = engine.New(engine.WithPreparedTeamMessaging(allowPreparedMessaging()))
	require.Error(t, err)
}

func TestPreparedTeamMessagingRejectsOutsideRoster(t *testing.T) {
	for _, receiver := range []string{"judge", "b@remote"} {
		t.Run(receiver, func(t *testing.T) {
			f := newPreparedTeamFixture(t, orchestration.StrategySequential)
			refreshPreparedTeam(t, &f, engine.WithA2A(a2a.Options{}), engine.WithPreparedTeamMessaging(allowPreparedMessaging()))
			attempted := false
			f.client.complete = func(_ context.Context, req *llm.Request, _ int) (*llm.Response, error) {
				if req.System == "frozen a" && !attempted {
					attempted = true
					return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "outside", Name: "agent_ask", Arguments: `{"to":"` + receiver + `","content":"private"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
				}
				return &llm.Response{Content: "without delegation", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
			}
			parent, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
			require.NoError(t, err)
			require.Len(t, parent.AgentRunIDs, 2)
			global, err := f.s.ListMessages(f.ctx, nil)
			require.NoError(t, err)
			require.Empty(t, global)
			private, err := a2a.NewRoutingStore(f.s, parent.ID)
			require.NoError(t, err)
			messages, err := private.ListMessages(f.ctx, nil)
			require.NoError(t, err)
			require.Empty(t, messages)
		})
	}
}

func TestPreparedTeamNestedPrivateAsksUseOneCoordinator(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategyParallel)
	refreshPreparedTeam(t, &f, engine.WithA2A(a2a.Options{}), engine.WithPreparedTeamMessaging(allowPreparedMessaging()))
	calls := map[string]int{}
	f.client.complete = func(_ context.Context, req *llm.Request, _ int) (*llm.Response, error) {
		calls[req.System]++
		target := ""
		if calls[req.System] == 1 {
			switch req.System {
			case "frozen a":
				target = "b"
			case "frozen b":
				target = "judge"
			}
		}
		if target != "" {
			return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "ask-" + target, Name: "agent_ask", Arguments: `{"to":"` + target + `","content":"check"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
		}
		return &llm.Response{Content: "checked evidence", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	ctx, cancel := context.WithTimeout(f.ctx, 20*time.Second)
	defer cancel()
	parent, err := f.e.RunPreparedOrchestration(ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.NoError(t, err)
	require.Equal(t, orchestration.StatusCompleted, parent.Status)
	require.Len(t, parent.AgentRunIDs, 5)
	require.Equal(t, 7, f.client.calls)
	account, err := f.s.GetOrchestrationBudgetSnapshot(ctx, parent.ID)
	require.NoError(t, err)
	require.EqualValues(t, 5, account.Charged.FanOut)
}

func TestPreparedTeamPrivateMessagesCannotExpandSharedBudget(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	refreshPreparedTeam(t, &f, engine.WithA2A(a2a.Options{}), engine.WithPreparedTeamMessaging(allowPreparedMessaging()))
	f.policy.Limits.FanOut = 1
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "ask-b", Name: "agent_ask", Arguments: `{"to":"b","content":"check"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	parent, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, execution.ErrBudgetExceeded)
	require.Equal(t, orchestration.StatusFailed, parent.Status)
	require.Equal(t, 1, f.client.calls)
	account, err := f.s.GetOrchestrationBudgetSnapshot(f.ctx, parent.ID)
	require.NoError(t, err)
	require.EqualValues(t, 1, account.Charged.FanOut)
	require.Len(t, parent.AgentRunIDs, 1)
}

func TestPreparedTeamPrivateRecipientRequiresCurrentRunGrant(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	config := allowPreparedMessaging()
	denied := errors.New("recipient permission denied")
	var asker id.AgentRunID
	config.AuthorizeRun = func(_ context.Context, _ id.OrchestrationID, key id.AgentRunID) error {
		if asker.IsNil() {
			asker = key
		}
		if key != asker {
			return denied
		}
		return nil
	}
	refreshPreparedTeam(t, &f, engine.WithA2A(a2a.Options{}), engine.WithPreparedTeamMessaging(config))
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "ask-b", Name: "agent_ask", Arguments: `{"to":"b","content":"check"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	parent, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, denied)
	require.Equal(t, orchestration.StatusFailed, parent.Status)
	require.Equal(t, 1, f.client.calls, "denied recipient must not reach the provider")
	private, err := a2a.NewRoutingStore(f.s, parent.ID)
	require.NoError(t, err)
	messages, err := private.ListMessages(f.ctx, nil)
	require.NoError(t, err)
	for _, message := range messages {
		require.NotContains(t, message.Content, denied.Error(), "private host diagnostics must not become peer output")
	}
}

type privateAskerReadGuard struct {
	*sqlite.Store
	asker   id.AgentRunID
	revoked bool
	reads   int
}

func (s *privateAskerReadGuard) GetRun(ctx context.Context, key id.AgentRunID) (*run.Run, error) {
	if s.revoked && key == s.asker {
		s.reads++
	}
	return s.Store.GetRun(ctx, key)
}

func TestPreparedTeamPrivateReplyChecksRunGrantBeforeReadingAsker(t *testing.T) {
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	guard := &privateAskerReadGuard{Store: f.s}
	denied := errors.New("asker permission revoked")
	config := allowPreparedMessaging()
	config.AuthorizeRun = func(_ context.Context, _ id.OrchestrationID, key id.AgentRunID) error {
		if guard.asker.IsNil() {
			guard.asker = key
		}
		if guard.revoked && key == guard.asker {
			return denied
		}
		return nil
	}
	refreshPreparedTeam(t, &f, engine.WithStore(guard), engine.WithA2A(a2a.Options{}), engine.WithPreparedTeamMessaging(config))
	f.client.complete = func(_ context.Context, req *llm.Request, _ int) (*llm.Response, error) {
		if req.System == "frozen a" {
			return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "ask-b", Name: "agent_ask", Arguments: `{"to":"b","content":"check"}`}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
		}
		guard.revoked = true
		return &llm.Response{Content: "private answer", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	_, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, denied)
	require.Equal(t, 2, f.client.calls)
	require.Zero(t, guard.reads, "revoked asker content must not be read to assemble a reply")
}
