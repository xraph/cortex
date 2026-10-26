package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

func TestAgentReplyBindsOriginalQuestion(t *testing.T) {
	for _, mode := range []string{"same_pause", "later_question", "missing_binding", "legacy_bypass"} {
		t.Run(mode, func(t *testing.T) {
			e, st, ctx := a2aEngine(t, []llm.ToolCall{{ID: "call-1", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"status?"}`}})
			current, err := e.RunAgent(ctx, "planner", "ask", nil)
			require.NoError(t, err)
			paused, err := st.GetSuspension(ctx, current.ID)
			require.NoError(t, err)
			messages, err := st.ListMessages(ctx, &a2a.MessageListFilter{Limit: 10})
			require.NoError(t, err)
			require.Len(t, messages, 1)
			question := messages[0]
			require.Equal(t, question.ReplyWith, paused.Pending[0].ReplyWith)
			if mode == "later_question" || mode == "missing_binding" {
				require.NoError(t, st.DeleteSuspension(ctx, current.ID))
				paused.ID = id.NewSuspensionID()
				paused.Pending[0].ReplyWith = ""
				if mode == "later_question" {
					paused.Pending[0].ReplyWith = id.NewMessageID().String()
				}
				require.NoError(t, st.CreateSuspension(ctx, paused))
			}
			if mode == "legacy_bypass" {
				err = (a2aResumer{eng: e}).ResumeAgentReply(ctx, current.ID, "call-1", "old answer")
			} else {
				_, err = e.A2A().Send(ctx, a2a.SendParams{Sender: a2a.Address{Agent: "worker"}, Receivers: []a2a.Address{{Agent: "planner"}}, Performative: a2a.Inform, Content: "answer", ConversationID: question.ConversationID, InReplyTo: question.ReplyWith})
			}
			if mode == "same_pause" {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, a2a.ErrAskResolutionConflict)
			}
			stored, readErr := st.GetRun(ctx, current.ID)
			require.NoError(t, readErr)
			if mode == "same_pause" {
				require.Equal(t, run.StateCompleted, stored.State)
			} else {
				require.Equal(t, run.StatePaused, stored.State)
				require.Equal(t, current.Revision, stored.Revision)
			}
		})
	}
}

type resolutionReadProbe struct {
	store.Store
	a2a.AskResolutionStore
	suspension.VersionedStore
	afterRead func(context.Context) error
}

func (p *resolutionReadProbe) GetAskResolution(ctx context.Context, token string) (a2a.AskResolution, error) {
	result, err := p.AskResolutionStore.GetAskResolution(ctx, token)
	if err == nil && p.afterRead != nil {
		err = p.afterRead(ctx)
	}
	return result, err
}

func TestAgentReplyRefusesChangedPauseAfterReadingAnswers(t *testing.T) {
	for _, mode := range []string{"replacement", "revision", "read_failure"} {
		t.Run(mode, func(t *testing.T) {
			e, st, ctx := a2aEngine(t, []llm.ToolCall{{ID: "call-1", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"status?"}`}})
			current, err := e.RunAgent(ctx, "planner", "ask", nil)
			require.NoError(t, err)
			messages, err := st.ListMessages(ctx, &a2a.MessageListFilter{Limit: 10})
			require.NoError(t, err)
			require.Len(t, messages, 1)
			native, ok := st.(a2a.AskResolutionStore)
			require.True(t, ok)
			versioned, ok := st.(suspension.VersionedStore)
			require.True(t, ok)
			fault := errors.New("answer read unavailable")
			probe := &resolutionReadProbe{Store: st, AskResolutionStore: native, VersionedStore: versioned}
			probe.afterRead = func(tc context.Context) error {
				switch mode {
				case "replacement":
					paused, readErr := st.GetSuspension(tc, current.ID)
					require.NoError(t, readErr)
					require.NoError(t, st.DeleteSuspension(tc, current.ID))
					paused.ID = id.NewSuspensionID()
					require.NoError(t, st.CreateSuspension(tc, paused))
				case "revision":
					stored, readErr := st.GetRun(tc, current.ID)
					require.NoError(t, readErr)
					require.NoError(t, st.UpdateRun(tc, stored))
				case "read_failure":
					return fault
				}
				return nil
			}
			e.store = probe
			question := messages[0]
			_, err = e.A2A().Send(ctx, a2a.SendParams{Sender: a2a.Address{Agent: "worker"}, Receivers: []a2a.Address{{Agent: "planner"}}, Performative: a2a.Inform, Content: "answer", ConversationID: question.ConversationID, InReplyTo: question.ReplyWith})
			switch mode {
			case "replacement":
				require.ErrorIs(t, err, cortex.ErrNotSuspended)
			case "revision":
				require.ErrorIs(t, err, cortex.ErrRunConflict)
			case "read_failure":
				require.ErrorIs(t, err, fault)
			}
			stored, readErr := st.GetRun(ctx, current.ID)
			require.NoError(t, readErr)
			require.Equal(t, run.StatePaused, stored.State)
			retained, readErr := native.GetAskResolution(ctx, question.ReplyWith)
			require.NoError(t, readErr)
			require.Contains(t, retained.Payload, "answer")
		})
	}
}

func TestAgentReplyWaitsForEveryPendingQuestion(t *testing.T) {
	e, st, ctx := a2aEngine(t, []llm.ToolCall{
		{ID: "call-1", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"first?"}`},
		{ID: "call-2", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"second?"}`},
	})
	current, err := e.RunAgent(ctx, "planner", "ask both", nil)
	require.NoError(t, err)
	messages, err := st.ListMessages(ctx, &a2a.MessageListFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, messages, 2)
	for i, question := range messages {
		_, err = e.A2A().Send(ctx, a2a.SendParams{Sender: a2a.Address{Agent: "worker"}, Receivers: []a2a.Address{{Agent: "planner"}}, Performative: a2a.Inform, Content: "answer", ConversationID: question.ConversationID, InReplyTo: question.ReplyWith})
		require.NoError(t, err)
		stored, readErr := st.GetRun(ctx, current.ID)
		require.NoError(t, readErr)
		if i == 0 {
			require.Equal(t, run.StatePaused, stored.State)
			// The next answer is handled by a fresh engine. The first answer
			// must come from its retained receipt, not process memory.
			e, err = New(WithStore(st), WithLLM(&scriptedLLM{answered: true}), WithA2A(a2a.Options{HopCeiling: 4, Workers: 1}))
			require.NoError(t, err)
		} else {
			require.Equal(t, run.StateCompleted, stored.State)
		}
	}
}

func TestAgentReplyBoundsCombinedAnswersBeforeClaim(t *testing.T) {
	e, st, ctx := a2aEngine(t, []llm.ToolCall{
		{ID: "call-1", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"first?"}`},
		{ID: "call-2", Name: toolAgentAsk, Arguments: `{"to":"worker","content":"second?"}`},
	})
	current, err := e.RunAgent(ctx, "planner", "ask both", nil)
	require.NoError(t, err)
	messages, err := st.ListMessages(ctx, &a2a.MessageListFilter{Limit: 10})
	require.NoError(t, err)
	require.Len(t, messages, 2)
	for i, question := range messages {
		_, err = e.A2A().Send(ctx, a2a.SendParams{Sender: a2a.Address{Agent: "worker"}, Receivers: []a2a.Address{{Agent: "planner"}}, Performative: a2a.Inform, Content: strings.Repeat("x", 3<<20), ConversationID: question.ConversationID, InReplyTo: question.ReplyWith})
		if i == 0 {
			require.NoError(t, err)
		} else {
			require.ErrorIs(t, err, safety.ErrLimit)
		}
	}
	stored, err := st.GetRun(ctx, current.ID)
	require.NoError(t, err)
	require.Equal(t, run.StatePaused, stored.State)
	require.Equal(t, current.Revision, stored.Revision)
}
