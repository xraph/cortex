package coordination_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/store/sqlite"
)

func busHandoff(t *testing.T) coordination.Handoff {
	t.Helper()
	h, err := coordination.SealHandoff(coordination.Handoff{SchemaVersion: 1, ID: id.New(coordination.PrefixHandoff), AssignmentID: id.New(coordination.PrefixAssignment), Fence: 1, Scope: cortex.ScopeFromContext(testCtx()), PlanID: id.New(planning.PrefixPlan), PlanRevision: 1, PlanDigest: evidence.Digest([]byte("plan")), TaskID: id.New(planning.PrefixTask), ParentRunID: id.NewAgentRunID(), Sender: coordination.Identity{Agent: "planner", Principal: "host"}, Receiver: coordination.Identity{Agent: "w1", Principal: "worker"}, Purpose: coordination.HandoffWork, GrantRef: "grant-1", Authority: planning.Authority{Budget: execution.Budget{MaxSteps: 5, MaxTokens: 100, MaxToolCalls: 2, Deadline: testNow.Add(time.Minute)}}, ContractDigest: evidence.Digest([]byte("contract")), Criteria: []string{"checked"}, Deadline: testNow.Add(time.Minute)})
	require.NoError(t, err)
	return h
}
func TestHandoffWithoutHostReceiverNeverRunsAgent(t *testing.T) {
	b, _, runner := newHandoffBus(t, nil)
	params, err := coordination.HandoffSendParams(busHandoff(t), "")
	require.NoError(t, err)
	_, err = b.Send(testCtx(), params)
	require.ErrorIs(t, err, coordination.ErrHandoffUnavailable)
	_, err = b.Drain(testCtx())
	require.NoError(t, err)
	require.Zero(t, runner.callCount())
}
func TestHandoffRejectsEnvelopeDowngradeAndIdentityChanges(t *testing.T) {
	for _, mode := range []string{"version", "language", "sender", "receiver", "broadcast", "scope", "deadline", "parent", "performative", "extra_json"} {
		t.Run(mode, func(t *testing.T) {
			b, _, runner := newHandoffBus(t, nil)
			params, err := coordination.HandoffSendParams(busHandoff(t), "")
			require.NoError(t, err)
			ctx := testCtx()
			switch mode {
			case "version":
				params.Protocol = "cortex-task-handoff/2"
			case "language":
				params.Language = "text/plain"
			case "sender":
				params.Sender.Agent = "forged"
			case "receiver":
				params.Receivers[0].Agent = "other"
			case "broadcast":
				params.Receivers = append(params.Receivers, a2a.Address{Agent: "other"})
			case "scope":
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "other"}}})
			case "deadline":
				deadline := testNow.Add(time.Hour)
				params.ReplyBy = &deadline
			case "parent":
				params.OriginRunID = id.NewAgentRunID()
			case "performative":
				params.Performative = a2a.Request
			case "extra_json":
				params.Content += " {}"
			}
			_, err = b.Send(ctx, params)
			require.ErrorIs(t, err, coordination.ErrInvalidHandoff)
			_, err = b.Drain(ctx)
			require.NoError(t, err)
			require.Zero(t, runner.callCount())
		})
	}
}
func TestHandoffLegacyTextMessageStillSupported(t *testing.T) {
	b, _, runner := newHandoffBus(t, nil)
	_, err := b.Send(testCtx(), a2a.SendParams{Sender: a2a.Address{Agent: "planner"}, Receivers: []a2a.Address{{Agent: "w1"}}, Performative: a2a.Request, Content: "ordinary request"})
	require.NoError(t, err)
	_, err = b.Drain(testCtx())
	require.NoError(t, err)
	require.Equal(t, 1, runner.callCount())
}
func TestHandoffHostFailureNeverFallsBackToAgent(t *testing.T) {
	// A structurally valid proposal still needs the host's current plan and grant.
	receiver, err := coordination.NewHandoffReceiver(coordination.HandoffReceiverConfig{MaxConcurrent: 1, Now: func() time.Time { return testNow }, Resolve: func(context.Context, coordination.Handoff) (coordination.HandoffContext, error) {
		return coordination.HandoffContext{}, errors.New("private authority error")
	}, Accept: func(context.Context, coordination.Handoff) error { t.Fatal("invalid proposal accepted"); return nil }})
	require.NoError(t, err)
	b, st, runner := newHandoffBus(t, receiver)
	params, err := coordination.HandoffSendParams(busHandoff(t), "")
	require.NoError(t, err)
	result, err := b.Send(testCtx(), params)
	require.NoError(t, err)
	_, err = b.Drain(testCtx())
	require.NoError(t, err)
	delivery, err := st.GetDelivery(testCtx(), result.Deliveries[0].DeliveryID)
	require.NoError(t, err)
	require.Equal(t, a2a.DeliveryFailed, delivery.State)
	require.Equal(t, coordination.ErrHandoffUnavailable.Error(), delivery.Error)
	require.Zero(t, runner.callCount())
}

var testNow = time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)

func testCtx() context.Context {
	return cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "one"}}})
}

type handoffRunner struct{ calls int }

func (r *handoffRunner) RunAgent(context.Context, string, string, *cortex.RunOpts) (*cortex.AgentResult, error) {
	r.calls++
	return &cortex.AgentResult{RunID: id.NewAgentRunID(), Output: "text reply"}, nil
}
func (r *handoffRunner) callCount() int { return r.calls }

type handoffClock struct{}

func (handoffClock) Now() time.Time { return testNow }
func newHandoffBus(t *testing.T, receiver *coordination.HandoffReceiver) (*a2a.Bus, *sqlite.Store, *handoffRunner) {
	t.Helper()
	drv := sqlitedriver.New()
	require.NoError(t, drv.Open(context.Background(), filepath.Join(t.TempDir(), "handoffs.db")))
	db, err := grove.Open(drv)
	require.NoError(t, err)
	st := sqlite.New(db)
	require.NoError(t, st.Migrate(context.Background()))
	t.Cleanup(func() { require.NoError(t, st.Close()) })
	handler, err := coordination.NewA2AHandoffHandler("", receiver, func() time.Time { return testNow })
	require.NoError(t, err)
	runner := &handoffRunner{}
	bus, err := a2a.NewBus(a2a.BusConfig{Store: st, Runner: runner, Clock: handoffClock{}, Synchronous: true, HandoffHandler: handler})
	require.NoError(t, err)
	return bus, st, runner
}

func TestHandoffClosedConversationDoesNotReachHost(t *testing.T) {
	resolved := 0
	receiver, err := coordination.NewHandoffReceiver(coordination.HandoffReceiverConfig{MaxConcurrent: 1, Now: func() time.Time { return testNow }, Resolve: func(context.Context, coordination.Handoff) (coordination.HandoffContext, error) {
		resolved++
		return coordination.HandoffContext{}, errors.New("must not resolve cancelled assignment")
	}, Accept: func(context.Context, coordination.Handoff) error { return nil }})
	require.NoError(t, err)
	b, st, runner := newHandoffBus(t, receiver)
	params, err := coordination.HandoffSendParams(busHandoff(t), "")
	require.NoError(t, err)
	result, err := b.Send(testCtx(), params)
	require.NoError(t, err)
	conversation, err := st.GetConversation(testCtx(), result.ConversationID)
	require.NoError(t, err)
	conversation.Status = a2a.StatusClosed
	require.NoError(t, st.UpdateConversation(testCtx(), conversation))
	_, err = b.Drain(testCtx())
	require.NoError(t, err)
	require.Zero(t, resolved)
	require.Zero(t, runner.callCount())
}
