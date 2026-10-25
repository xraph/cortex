package a2a

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/id"
)

func TestAskReplyMembership(t *testing.T) {
	for _, mode := range []string{"outsider", "wrong_target", "remote_identity", "duplicate_question", "cross_conversation"} {
		t.Run(mode, func(t *testing.T) {
			b, _, _, resumed, _, _ := newTestBus(t)
			b.opts.HopCeiling = 16
			ctx := testCtx()
			ask, err := b.Ask(ctx, AskParams{SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "worker"}}, Performative: Request, Content: "review"}, AskerRunID: id.NewAgentRunID(), ToolCallID: "review"})
			require.NoError(t, err)
			reply := SendParams{Sender: Address{Agent: "worker"}, Receivers: []Address{{Agent: "planner"}}, Performative: Inform, Content: "unrequested answer", ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith}
			switch mode {
			case "outsider":
				reply.Sender.Agent = "outsider"
			case "wrong_target":
				reply.Receivers = []Address{{Agent: "bystander"}}
			case "remote_identity":
				reply.Sender.Node = "another-host"
			case "duplicate_question", "cross_conversation":
				question := SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "outsider"}}, Performative: Request, Content: "different question", ReplyWith: ask.ReplyWith}
				if mode == "duplicate_question" {
					question.ConversationID = ask.ConversationID
				}
				other, sendErr := b.Send(ctx, question)
				require.NoError(t, sendErr)
				reply.ConversationID = other.ConversationID
				reply.Sender.Agent = "outsider"
			}
			_, err = b.Send(ctx, reply)
			require.NoError(t, err)
			require.Zero(t, resumed.count(), "unrequested replies must not claim the original ask")
			_, err = b.Send(ctx, SendParams{Sender: Address{Agent: "worker"}, Receivers: []Address{{Agent: "planner"}}, Performative: Inform, Content: "reviewed", ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith})
			require.NoError(t, err)
			require.Equal(t, 1, resumed.count())
			payload := decodeAskReply(t, resumed.last().Result)
			require.True(t, payload.Complete)
			require.Len(t, payload.Replies, 1)
			require.Equal(t, "reviewed", payload.Replies[0].Content)
		})
	}
}

func TestAskTimeoutExcludesUnrequestedReplies(t *testing.T) {
	b, _, _, resumed, _, clk := newTestBus(t)
	ctx := testCtx()
	ask, err := b.Ask(ctx, AskParams{SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "worker"}, {Agent: "second"}}, Performative: Request, Content: "review"}, AskerRunID: id.NewAgentRunID(), ToolCallID: "review"})
	require.NoError(t, err)
	_, err = b.Send(ctx, SendParams{Sender: Address{Agent: "outsider"}, Receivers: []Address{{Agent: "planner"}}, Performative: Inform, Content: "unrequested answer", ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith})
	require.NoError(t, err)
	require.Zero(t, resumed.count())
	clk.advance(DefaultReplyBy + time.Minute)
	_, err = b.SweepExpiredAsks(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, resumed.count())
	require.NotContains(t, resumed.last().Result, "unrequested answer")
}
