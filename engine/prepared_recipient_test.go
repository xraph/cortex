package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/orchestration"
	"github.com/xraph/cortex/run"
)

type preparedRecipientObserver func(context.Context, a2a.RecipientRequest) (a2a.RecipientObservation, error)

func (f preparedRecipientObserver) ObserveRecipient(ctx context.Context, req a2a.RecipientRequest) (a2a.RecipientObservation, error) {
	return f(ctx, req)
}

type observationOnlyRunner struct{ calls int }

func (r *observationOnlyRunner) ReserveAgent(context.Context, id.OrchestrationID, string, string, *cortex.RunOpts) (*cortex.ReservedInvocation, error) {
	r.calls++
	return nil, cortex.ErrRunIncomplete
}

func TestPreparedRecipientObservationAfterIndependentResume(t *testing.T) {
	f, request := preparedRecipientFixture(t)
	authorize := func(context.Context, id.OrchestrationID, id.AgentRunID) error { return nil }
	runner := &observationOnlyRunner{}
	observerEngine := f.e
	busConfig := a2a.BusConfig{Store: f.s, ReservedRunner: runner, OrchestrationID: request.OrchestrationID, Synchronous: true,
		RecipientObserver: preparedRecipientObserver(func(ctx context.Context, req a2a.RecipientRequest) (a2a.RecipientObservation, error) {
			return observerEngine.ObservePreparedRecipient(ctx, req, authorize)
		}),
	}
	bus, err := a2a.NewBus(busConfig)
	require.NoError(t, err)
	sent, err := bus.Send(f.ctx, a2a.SendParams{Sender: a2a.Address{Agent: "reviewer"}, Receivers: []a2a.Address{{Agent: request.AgentName}}, Performative: a2a.Request, Content: "work"})
	require.NoError(t, err)
	private, err := a2a.NewRoutingStore(f.s, request.OrchestrationID)
	require.NoError(t, err)
	claimed, err := private.ClaimDelivery(f.ctx, sent.Deliveries[0].DeliveryID)
	require.NoError(t, err)
	binder, available := private.(a2a.DeliveryRunBinder)
	require.True(t, available)
	require.NoError(t, binder.BindDeliveryRun(f.ctx, claimed, request.RunID))
	allowReply := func(context.Context, id.OrchestrationID, id.DeliveryID) error { return nil }
	noReply, err := bus.RecoverRecipient(f.ctx, claimed.ID, allowReply)
	require.NoError(t, err)
	require.Zero(t, noReply)
	waiting, err := f.e.ObservePreparedRecipient(f.ctx, request, authorize)
	require.NoError(t, err)
	require.Equal(t, a2a.RecipientWaiting, waiting.State)
	require.Empty(t, waiting.Output)
	require.Equal(t, 1, f.client.calls)
	_, err = f.e.Resume(f.ctx, request.RunID, engine.ResumeInput{ToolResults: []engine.ToolResult{{ToolCallID: "await", Content: "ready"}}})
	require.NoError(t, err)
	fresh, err := engine.New(engine.WithStore(f.s), engine.WithExternalTool(llm.Tool{Name: "await"}))
	require.NoError(t, err)
	observerEngine = fresh
	freshBus, err := a2a.NewBus(busConfig)
	require.NoError(t, err)
	reply, err := freshBus.RecoverRecipient(f.ctx, claimed.ID, allowReply)
	require.NoError(t, err)
	require.True(t, reply.Fresh)
	_, err = freshBus.Drain(f.ctx)
	require.NoError(t, err)
	inbox, err := freshBus.Inbox(f.ctx, "reviewer", a2a.InboxFilter{UnreadOnly: true})
	require.NoError(t, err)
	require.Len(t, inbox, 1)
	require.Equal(t, "retained completion", inbox[0].Content)
	require.Zero(t, runner.calls)
	completed, err := fresh.ObservePreparedRecipient(f.ctx, request, authorize)
	require.NoError(t, err)
	require.Equal(t, a2a.RecipientSucceeded, completed.State)
	require.Equal(t, "retained completion", completed.Output)
	require.Equal(t, 2, f.client.calls, "observation cannot call the provider")
	denied := errors.New("recovery denied")
	checks := 0
	_, err = fresh.ObservePreparedRecipient(f.ctx, request, func(context.Context, id.OrchestrationID, id.AgentRunID) error {
		checks++
		if checks > 1 {
			return denied
		}
		return nil
	})
	require.ErrorIs(t, err, denied)
	_, err = fresh.ObservePreparedRecipient(f.ctx, request, nil)
	require.Error(t, err)
	wrong := request
	wrong.AgentName = "missing participant"
	_, err = fresh.ObservePreparedRecipient(f.ctx, wrong, authorize)
	require.Error(t, err)
	wrong = request
	wrong.RunID = id.NewAgentRunID()
	_, err = fresh.ObservePreparedRecipient(f.ctx, wrong, authorize)
	require.Error(t, err)
	foreign := cortex.WithScope(f.ctx, cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "foreign"}}})
	_, err = fresh.ObservePreparedRecipient(foreign, request, authorize)
	require.Error(t, err)
	_, err = fresh.ObservePreparedRecipient(context.Background(), request, authorize)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	require.Equal(t, 2, f.client.calls)
}

func preparedRecipientFixture(t *testing.T) (preparedTeamFixture, a2a.RecipientRequest) {
	t.Helper()
	f := newPreparedTeamFixture(t, orchestration.StrategySequential)
	refreshPreparedTeam(t, &f, engine.WithExternalTool(llm.Tool{Name: "await"}))
	f.client.complete = func(context.Context, *llm.Request, int) (*llm.Response, error) {
		if f.client.calls == 1 {
			return &llm.Response{ToolCalls: []llm.ToolCall{{ID: "await", Name: "await", Arguments: "{}"}}, FinishReason: "tool_calls", Usage: llm.Usage{TotalTokens: 10}}, nil
		}
		return &llm.Response{Content: "retained completion", FinishReason: "stop", Usage: llm.Usage{TotalTokens: 10}}, nil
	}
	team, err := f.e.RunPreparedOrchestration(f.ctx, f.p, "work", engine.PreparedRunOptions{OrchestrationBudget: &f.policy})
	require.ErrorIs(t, err, cortex.ErrOrchestrationWaiting)
	child, err := f.s.GetRun(f.ctx, team.AgentRunIDs[0])
	require.NoError(t, err)
	view, err := f.p.Inspect()
	require.NoError(t, err)
	var name string
	for participant, captured := range view.Participants {
		inspected, inspectErr := captured.Inspect()
		require.NoError(t, inspectErr)
		if inspected.AgentID == child.AgentID {
			name = participant
		}
	}
	require.NotEmpty(t, name)
	request := a2a.RecipientRequest{OrchestrationID: team.ID, RunID: child.ID, AgentName: name}
	return f, request
}

func TestPreparedRecipientSettledTerminalObservation(t *testing.T) {
	for _, state := range []run.State{run.StateFailed, run.StateCancelled, run.StateIncomplete, run.StatePaused} {
		t.Run(string(state), func(t *testing.T) {
			f, req := preparedRecipientFixture(t)
			child, err := f.s.GetRun(f.ctx, req.RunID)
			require.NoError(t, err)
			child.State, child.Output = state, "private diagnostic must not escape"
			require.NoError(t, f.s.UpdateRun(f.ctx, child))
			reservationID, err := id.Parse(child.Provenance.OrchestrationReservationID)
			require.NoError(t, err)
			reserved, err := f.s.GetReservation(f.ctx, reservationID)
			require.NoError(t, err)
			require.NoError(t, f.s.SettleOrchestrationChild(f.ctx, execution.Settlement{ReservationID: reservationID, RequestDigest: reserved.RequestDigest, Usage: execution.ReservationUsage{Actual: execution.Resources{Tokens: int64(child.Usage.Tokens)}, Known: execution.KnownTokens}, Receipt: "host:terminal-observation"}))
			result, err := f.e.ObservePreparedRecipient(f.ctx, req, func(context.Context, id.OrchestrationID, id.AgentRunID) error { return nil })
			if state == run.StatePaused {
				require.Error(t, err)
				require.Zero(t, result)
			} else {
				require.NoError(t, err)
				require.Equal(t, a2a.RecipientFailed, result.State)
				require.Empty(t, result.Output)
			}
			require.Equal(t, 1, f.client.calls)
		})
	}
}
