package grpcbind_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/a2aremote"
	"github.com/xraph/cortex/a2aremote/grpcbind"
	"github.com/xraph/cortex/a2aremote/grpcbind/a2apb"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/skill"
)

// gateway is the smallest thing that satisfies the seam: enough to prove
// the binding translates, not enough to test cortex itself.
type gateway struct {
	sent   []a2a.SendParams
	runs   map[string]*run.Run
	agents map[string]bool
}

func newGateway() *gateway {
	return &gateway{runs: map[string]*run.Run{}, agents: map[string]bool{"worker": true}}
}

func (g *gateway) SendMessage(_ context.Context, p a2a.SendParams) (*a2a.SendResult, error) {
	g.sent = append(g.sent, p)
	return &a2a.SendResult{
		MessageID:      id.NewMessageID(),
		ConversationID: id.NewConversationID(),
		Deliveries:     []a2a.DeliveryOutcome{{Receiver: p.Receivers[0], DeliveryID: id.NewDeliveryID()}},
	}, nil
}

func (g *gateway) GetRun(_ context.Context, runID id.AgentRunID) (*run.Run, error) {
	r, ok := g.runs[runID.String()]
	if !ok {
		return nil, errors.New("not found")
	}
	return r, nil
}

func (g *gateway) GetDelivery(context.Context, id.DeliveryID) (*a2a.Delivery, error) {
	return nil, errors.New("not found")
}

func (g *gateway) ListRuns(context.Context, *run.ListFilter) ([]*run.Run, error) {
	out := make([]*run.Run, 0, len(g.runs))
	for _, r := range g.runs {
		out = append(out, r)
	}
	return out, nil
}

func (g *gateway) CancelRun(context.Context, id.AgentRunID) error { return nil }

func (g *gateway) GetAgentByName(_ context.Context, name string) (*agent.Config, error) {
	if !g.agents[name] {
		return nil, errors.New("no such agent")
	}
	return &agent.Config{ID: id.NewAgentID(), Name: name}, nil
}

func (g *gateway) GetSkillByName(context.Context, string) (*skill.Skill, error) {
	return nil, errors.New("no such skill")
}

func dial(t *testing.T, gw a2aremote.Gateway, res a2aremote.PeerResolver) a2apb.A2AServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	grpcbind.Register(srv, a2aremote.NewService(gw, res, a2aremote.Options{}))

	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return a2apb.NewA2AServiceClient(conn)
}

func okResolver() a2aremote.PeerResolver {
	return a2aremote.ResolverFunc(func(_ context.Context, cred a2aremote.Credentials) (a2aremote.Peer, error) {
		if cred.Header("authorization") != "Bearer ok" {
			return a2aremote.Peer{}, errors.New("who are you")
		}
		return a2aremote.Peer{
			Node:  "peer.example",
			Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "acme"}}},
		}, nil
	})
}

func authed(ctx context.Context) context.Context {
	return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer ok")
}

func TestGRPCSendMessage(t *testing.T) {
	gw := newGateway()
	client := dial(t, gw, okResolver())

	res, err := client.SendMessage(authed(context.Background()), &a2apb.SendMessageRequest{
		Tenant: "worker",
		Message: &a2apb.Message{
			MessageId: "m1", Role: a2apb.Role_ROLE_USER,
			Parts: []*a2apb.Part{{Content: &a2apb.Part_Text{Text: "hello over grpc"}}},
		},
	})
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if res.GetTask() == nil {
		t.Fatalf("a request must come back as a task: %+v", res)
	}
	if len(gw.sent) != 1 || gw.sent[0].Content != "hello over grpc" {
		t.Fatalf("the gateway saw %+v", gw.sent)
	}
	// The sender is namespaced by the peer's node here exactly as it is
	// on the other two bindings, because the rule lives in the service.
	if gw.sent[0].Sender.Node != "peer.example" {
		t.Fatalf("sender = %+v, want it namespaced by the peer", gw.sent[0].Sender)
	}
}

func TestGRPCGetTask(t *testing.T) {
	gw := newGateway()
	runID := id.NewAgentRunID()
	gw.runs[runID.String()] = &run.Run{ID: runID, State: run.StateCompleted, Output: "done"}
	client := dial(t, gw, okResolver())

	task, err := client.GetTask(authed(context.Background()), &a2apb.GetTaskRequest{
		Tenant: "worker", Id: runID.String(),
	})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.GetStatus().GetState() != a2apb.TaskState_TASK_STATE_COMPLETED {
		t.Fatalf("state = %s", task.GetStatus().GetState())
	}
	if len(task.GetArtifacts()) != 1 || task.GetArtifacts()[0].GetParts()[0].GetText() != "done" {
		t.Fatalf("artifacts = %+v", task.GetArtifacts())
	}
}

// The status codes are the specification's own mapping, so a gRPC client
// can tell "not there" from "not allowed" without reading strings.
func TestGRPCErrorsCarryTheRightCode(t *testing.T) {
	gw := newGateway()
	client := dial(t, gw, okResolver())
	ctx := authed(context.Background())

	if _, err := client.GetTask(ctx, &a2apb.GetTaskRequest{Tenant: "worker", Id: "arun_notreal"}); status.Code(err) != codes.NotFound {
		t.Errorf("missing task: code = %s, want NotFound", status.Code(err))
	}
	if _, err := client.SendMessage(ctx, &a2apb.SendMessageRequest{
		Tenant:  "nobody",
		Message: &a2apb.Message{MessageId: "m", Role: a2apb.Role_ROLE_USER, Parts: []*a2apb.Part{{Content: &a2apb.Part_Text{Text: "x"}}}},
	}); status.Code(err) != codes.NotFound {
		t.Errorf("unknown tenant: code = %s, want NotFound", status.Code(err))
	}
}

func TestGRPCUnauthenticated(t *testing.T) {
	client := dial(t, newGateway(), okResolver())

	// No credentials in the metadata at all.
	_, err := client.ListTasks(context.Background(), &a2apb.ListTasksRequest{Tenant: "worker"})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("code = %s, want Unauthenticated", status.Code(err))
	}
}

func TestGRPCStreamingIsRefusedRatherThanHanging(t *testing.T) {
	client := dial(t, newGateway(), okResolver())

	stream, err := client.SendStreamingMessage(authed(context.Background()), &a2apb.SendMessageRequest{
		Tenant:  "worker",
		Message: &a2apb.Message{MessageId: "m", Role: a2apb.Role_ROLE_USER},
	})
	if err == nil {
		_, err = stream.Recv()
	}
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("code = %s, want Unimplemented for something the card says is not there", status.Code(err))
	}
}
