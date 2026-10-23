package a2aremote

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
)

func TestHandoffMappingPreservesExactDeadlineAndParentRun(t *testing.T) {
	deadline := time.Date(2026, 10, 23, 0, 0, 0, 123456789, time.UTC)
	parent := id.NewAgentRunID()
	e := &a2a.Envelope{ID: id.NewMessageID(), Performative: a2a.Propose, Protocol: a2a.HandoffProtocol, Language: a2a.HandoffLanguage, Encoding: "utf-8", ReplyBy: &deadline, OriginRunID: parent, Content: `{"proposal":"opaque sealed body"}`}
	message := MessageFromEnvelope(e)
	params, err := EnvelopeParamsFromMessage(message, a2a.Address{Agent: "sender", Node: "authenticated-peer"}, a2a.Address{Agent: "receiver"})
	require.NoError(t, err)
	require.Equal(t, parent, params.OriginRunID)
	require.Equal(t, deadline, *params.ReplyBy)
	require.Equal(t, e.Content, params.Content)
	require.Equal(t, "authenticated-peer", params.Sender.Node)
	message.TaskID = "invalid"
	_, err = EnvelopeParamsFromMessage(message, params.Sender, params.Receivers[0])
	require.Error(t, err)
}
func TestHandoffAcknowledgementDoesNotClaimTaskSuccess(t *testing.T) {
	gw := newFakeGateway()
	svc := testService(t, gw, okResolver())
	deadline := time.Now().Add(time.Minute)
	e := &a2a.Envelope{ID: id.NewMessageID(), Performative: a2a.Propose, Protocol: a2a.HandoffProtocol, Language: a2a.HandoffLanguage, Encoding: "utf-8", ReplyBy: &deadline, OriginRunID: id.NewAgentRunID(), Content: `{"proposal":"opaque sealed body"}`}
	res, err := svc.SendMessage(t.Context(), Credentials{}, SendMessageRequest{Tenant: "worker", Message: MessageFromEnvelope(e)})
	require.NoError(t, err)
	require.Nil(t, res.Task)
	require.NotNil(t, res.Message)
	require.Equal(t, "queued", res.Message.Parts[0].Text)
}
