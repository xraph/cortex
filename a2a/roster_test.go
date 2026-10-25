package a2a

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
)

type rosterMemoryStore struct{ *memStore }

func (*rosterMemoryStore) SupportsOrchestrationRouting() bool { return true }

func (*rosterMemoryStore) SupportsScopedOrchestrationDiscovery() bool { return true }

func TestRosterRejectsUnlistedEndpointsBeforeWriting(t *testing.T) {
	for _, endpoint := range []string{"sender", "receiver", "reply_to", "remote_sender", "remote_receiver", "scope"} {
		t.Run(endpoint, func(t *testing.T) {
			ctx := testCtx()
			st := &rosterMemoryStore{newMemStore()}
			roster := &Roster{Scope: cortex.ScopeFromContext(ctx), Participants: []RosterParticipant{{Name: "planner", AgentID: id.NewAgentID()}, {Name: "worker", AgentID: id.NewAgentID()}}}
			b, err := NewBus(BusConfig{Store: st, Runner: newFakeRunner(), OrchestrationID: id.NewOrchestrationID(), Roster: roster, Synchronous: true})
			require.NoError(t, err)
			message := SendParams{Sender: Address{Agent: "planner"}, Receivers: []Address{{Agent: "worker"}}, Performative: Inform, Content: "private"}
			switch endpoint {
			case "sender":
				message.Sender.Agent = "outsider"
			case "receiver":
				message.Receivers[0].Agent = "outsider"
			case "reply_to":
				message.ReplyTo = []Address{{Agent: "outsider"}}
			case "remote_sender":
				message.Sender.Node = "elsewhere"
			case "remote_receiver":
				message.Receivers[0].Node = "elsewhere"
			case "scope":
				ctx = cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}})
			}
			_, err = b.Send(ctx, message)
			require.Error(t, err)
			messages, err := st.ListMessages(ctx, nil)
			require.NoError(t, err)
			require.Empty(t, messages)
			conversations, err := st.ListConversations(ctx, nil)
			require.NoError(t, err)
			require.Empty(t, conversations)
		})
	}
}

func TestRosterRejectsForeignInboxAndQueuedDelivery(t *testing.T) {
	ctx := testCtx()
	st := &rosterMemoryStore{newMemStore()}
	runner := newFakeRunner()
	team := id.NewOrchestrationID()
	b, err := NewBus(BusConfig{Store: st, Runner: runner, OrchestrationID: team, Roster: &Roster{Scope: cortex.ScopeFromContext(ctx), Participants: []RosterParticipant{{Name: "planner", AgentID: id.NewAgentID()}, {Name: "worker", AgentID: id.NewAgentID()}}}, Synchronous: true})
	require.NoError(t, err)
	_, err = b.Inbox(ctx, "outsider", InboxFilter{})
	require.Error(t, err)
	msg := &Envelope{ID: id.NewMessageID(), Scope: cortex.ScopeFromContext(ctx), Sender: Address{Agent: "outsider"}, Receivers: []Address{{Agent: "worker"}}, Performative: Request, Content: "untrusted"}
	require.NoError(t, st.CreateMessage(ctx, msg))
	d := &Delivery{ID: id.NewDeliveryID(), Scope: msg.Scope, MessageID: msg.ID, Receiver: msg.Receivers[0], State: DeliveryQueued}
	require.NoError(t, st.CreateDelivery(ctx, d))
	_, err = b.Drain(ctx)
	require.NoError(t, err)
	stored, err := st.GetDelivery(ctx, d.ID)
	require.NoError(t, err)
	require.Equal(t, DeliveryFailed, stored.State)
	require.True(t, stored.RunID.IsNil())
	require.Zero(t, runner.callCount())
}

func TestRosterInboxRejectsUnlistedStoredSender(t *testing.T) {
	ctx := testCtx()
	st := &rosterMemoryStore{newMemStore()}
	b, err := NewBus(BusConfig{Store: st, Runner: newFakeRunner(), OrchestrationID: id.NewOrchestrationID(), Roster: &Roster{Scope: cortex.ScopeFromContext(ctx), Participants: []RosterParticipant{{Name: "worker", AgentID: id.NewAgentID()}}}, Synchronous: true})
	require.NoError(t, err)
	msg := &Envelope{ID: id.NewMessageID(), Scope: cortex.ScopeFromContext(ctx), Sender: Address{Agent: "outsider"}, Receivers: []Address{{Agent: "worker"}}, Performative: Inform, Content: "untrusted"}
	require.NoError(t, st.CreateMessage(ctx, msg))
	d := &Delivery{ID: id.NewDeliveryID(), Scope: msg.Scope, MessageID: msg.ID, Receiver: msg.Receivers[0], State: DeliveryDelivered}
	require.NoError(t, st.CreateDelivery(ctx, d))
	items, err := b.Inbox(ctx, "worker", InboxFilter{})
	require.ErrorIs(t, err, ErrRosterDenied)
	require.Empty(t, items)
	stored, err := st.GetDelivery(ctx, d.ID)
	require.NoError(t, err)
	require.Nil(t, stored.ReadAt)
}

func TestRosterFreezesIdentityAndHeldRecipients(t *testing.T) {
	ctx := testCtx()
	st := &rosterMemoryStore{newMemStore()}
	planner, worker := id.NewAgentID(), id.NewAgentID()
	roster := &Roster{Scope: cortex.ScopeFromContext(ctx), Participants: []RosterParticipant{{Name: "planner", AgentID: planner}, {Name: "worker", AgentID: worker}}}
	b, err := NewBus(BusConfig{Store: st, Runner: newFakeRunner(), OrchestrationID: id.NewOrchestrationID(), Roster: roster, Synchronous: true})
	require.NoError(t, err)
	roster.Participants[1] = RosterParticipant{Name: "outsider", AgentID: id.NewAgentID()}
	roster.Scope.Levels[0].Value = "changed"
	addr, err := b.ParticipantAddress(ctx, worker)
	require.NoError(t, err)
	require.Equal(t, Address{Agent: "worker"}, addr)
	_, err = b.ParticipantAddress(ctx, id.NewAgentID())
	require.ErrorIs(t, err, ErrRosterDenied)
	_, err = b.ParticipantAddress(context.Background(), planner)
	require.ErrorIs(t, err, cortex.ErrNoScope)
	recipients := []Address{{Agent: "worker"}}
	ask, err := b.Ask(ctx, AskParams{SendParams: SendParams{Sender: Address{Agent: "planner"}, Receivers: recipients, Performative: Request, Content: "review"}, AskerRunID: id.NewAgentRunID(), ToolCallID: "review", Hold: true})
	require.NoError(t, err)
	recipients[0].Agent = "outsider"
	require.NoError(t, b.Release(ctx, ask))
	queued, err := st.ListQueuedDeliveries(ctx, 10)
	require.NoError(t, err)
	require.Len(t, queued, 1)
	require.Equal(t, Address{Agent: "worker"}, queued[0].Receiver)
}

func TestRosterRejectsAmbiguousConfiguration(t *testing.T) {
	for _, mode := range []string{"global", "empty", "scope", "duplicate_name", "duplicate_id", "remote", "invalid_id"} {
		t.Run(mode, func(t *testing.T) {
			team := id.NewOrchestrationID()
			roster := &Roster{Scope: cortex.ScopeFromContext(testCtx()), Participants: []RosterParticipant{{Name: "planner", AgentID: id.NewAgentID()}, {Name: "worker", AgentID: id.NewAgentID()}}}
			switch mode {
			case "global":
				team = id.Nil
			case "empty":
				roster.Participants = nil
			case "scope":
				roster.Scope = cortex.Scope{}
			case "duplicate_name":
				roster.Participants[1].Name = roster.Participants[0].Name
			case "duplicate_id":
				roster.Participants[1].AgentID = roster.Participants[0].AgentID
			case "remote":
				roster.Participants[1].Name = "worker@remote"
			case "invalid_id":
				roster.Participants[1].AgentID = id.NewAgentRunID()
			}
			_, err := NewBus(BusConfig{Store: &rosterMemoryStore{newMemStore()}, Runner: newFakeRunner(), OrchestrationID: team, Roster: roster})
			require.ErrorIs(t, err, ErrRosterDenied)
		})
	}
}

type unscopedRosterStore struct{ *rosterMemoryStore }

func (*unscopedRosterStore) SupportsScopedOrchestrationDiscovery() bool { return false }

func TestRosterRequiresScopedDiscovery(t *testing.T) {
	roster := &Roster{Scope: cortex.ScopeFromContext(testCtx()), Participants: []RosterParticipant{{Name: "worker", AgentID: id.NewAgentID()}}}
	_, err := NewBus(BusConfig{Store: &unscopedRosterStore{&rosterMemoryStore{newMemStore()}}, Runner: newFakeRunner(), OrchestrationID: id.NewOrchestrationID(), Roster: roster})
	require.ErrorIs(t, err, ErrRoutingUnavailable)
}
