package storetest

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/store"
)

type rosterClock struct{ at time.Time }

func (c rosterClock) Now() time.Time { return c.at }

type rosterResumer struct {
	calls  int
	result string
}

func (r *rosterResumer) ResumeAgentReply(_ context.Context, _ id.AgentRunID, _, result string) error {
	r.calls++
	r.result = result
	return nil
}

func testA2ARosterDiscoveryScope(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, mode := range []string{"queue", "reclaim", "asks"} {
		t.Run(mode, func(t *testing.T) {
			backend := newStore(t)
			team := id.NewOrchestrationID()
			parent, child := ctxWithScope("roster-discovery"), ctxWithScope("roster-discovery", "child")
			clock := rosterClock{at: time.Now().Add(time.Hour)}
			makeBus := func(ctx context.Context) (*a2a.Bus, *rosterResumer) {
				r := &rosterResumer{}
				b, err := a2a.NewBus(a2a.BusConfig{Store: backend, Runner: routingRejectRunner{}, Resumer: r, Clock: clock, OrchestrationID: team, Roster: &a2a.Roster{Scope: cortex.ScopeFromContext(ctx), Participants: []a2a.RosterParticipant{{Name: "planner", AgentID: id.NewAgentID()}, {Name: "worker", AgentID: id.NewAgentID()}}}, Synchronous: true})
				require.NoError(t, err)
				return b, r
			}
			parentBus, parentResumer := makeBus(parent)
			childBus, childResumer := makeBus(child)
			private, err := a2a.NewRoutingStore(backend, team)
			require.NoError(t, err)
			if mode == "asks" {
				for _, entry := range []struct {
					ctx context.Context
					bus *a2a.Bus
				}{{parent, parentBus}, {child, childBus}} {
					deadline := time.Now().Add(-time.Minute)
					_, err = entry.bus.Ask(entry.ctx, a2a.AskParams{SendParams: a2a.SendParams{Sender: a2a.Address{Agent: "planner"}, Receivers: []a2a.Address{{Agent: "worker"}}, Performative: a2a.Request, Content: "review", ReplyBy: &deadline}, AskerRunID: id.NewAgentRunID(), ToolCallID: "review", Hold: true})
					require.NoError(t, err)
				}
				n, sweepErr := parentBus.SweepExpiredAsks(context.Background())
				require.NoError(t, sweepErr)
				require.Equal(t, 1, n)
				require.Equal(t, 1, parentResumer.calls)
				require.Zero(t, childResumer.calls)
				n, sweepErr = childBus.SweepExpiredAsks(context.Background())
				require.NoError(t, sweepErr)
				require.Equal(t, 1, n)
				require.Equal(t, 1, childResumer.calls)
				return
			}
			ids := make([]id.DeliveryID, 0, 2)
			for _, entry := range []struct {
				ctx context.Context
				bus *a2a.Bus
			}{{child, childBus}, {parent, parentBus}} {
				result, sendErr := entry.bus.Send(entry.ctx, a2a.SendParams{Sender: a2a.Address{Agent: "planner"}, Receivers: []a2a.Address{{Agent: "worker"}}, Performative: a2a.Inform, Content: "private"})
				require.NoError(t, sendErr)
				ids = append(ids, result.Deliveries[0].DeliveryID)
				if mode == "reclaim" {
					_, err = private.ClaimDelivery(entry.ctx, ids[len(ids)-1])
					require.NoError(t, err)
				}
			}
			var n int
			if mode == "queue" {
				n, err = parentBus.Drain(context.Background())
			} else {
				n, err = parentBus.ReclaimStaleDeliveries(context.Background())
			}
			require.NoError(t, err)
			require.Equal(t, 1, n)
			retained, err := private.GetDelivery(child, ids[0])
			require.NoError(t, err)
			want := a2a.DeliveryQueued
			if mode == "reclaim" {
				want = a2a.DeliveryDelivering
			}
			require.Equal(t, want, retained.State)
		})
	}
}

func testA2AReplyMembership(t *testing.T, newStore func(*testing.T) store.Store) {
	backend := newStore(t)
	ctx := ctxWithScope("reply-membership")
	resumed := &rosterResumer{}
	b, err := a2a.NewBus(a2a.BusConfig{Store: backend, Runner: routingRejectRunner{}, Resumer: resumed, Synchronous: true, Options: a2a.Options{HopCeiling: 16}})
	require.NoError(t, err)
	ask, err := b.Ask(ctx, a2a.AskParams{SendParams: a2a.SendParams{Sender: a2a.Address{Agent: "planner"}, Receivers: []a2a.Address{{Agent: "worker"}}, Performative: a2a.Request, Content: "review"}, AskerRunID: id.NewAgentRunID(), ToolCallID: "review", Hold: true})
	require.NoError(t, err)
	for _, entry := range []struct{ from, to string }{{"outsider", "planner"}, {"worker", "elsewhere"}, {"worker", "planner"}} {
		_, err = b.Send(ctx, a2a.SendParams{Sender: a2a.Address{Agent: entry.from}, Receivers: []a2a.Address{{Agent: entry.to}}, Performative: a2a.Inform, Content: entry.from + " to " + entry.to, ConversationID: ask.ConversationID, InReplyTo: ask.ReplyWith})
		require.NoError(t, err)
		if entry.from == "worker" && entry.to == "planner" {
			require.Equal(t, 1, resumed.calls)
		} else {
			require.Zero(t, resumed.calls)
		}
	}
	require.NotContains(t, resumed.result, "outsider")
	require.NotContains(t, resumed.result, "elsewhere")
}
