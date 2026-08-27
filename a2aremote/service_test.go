package a2aremote

import (
	"context"
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func okResolver() staticResolver {
	return staticResolver{peer: Peer{Node: "peer.example", Scope: testScope()}}
}

func plainRequest(text string) SendMessageRequest {
	return SendMessageRequest{
		Tenant:  "worker",
		Message: Message{MessageID: "m1", Role: RoleUser, Parts: []Part{{Text: text}}},
	}
}

// The load-bearing rule of the whole inbound path: the scope comes from
// the resolver and from nothing a caller can set.
func TestSendMessageUsesTheResolversScopeAndNotTheMessages(t *testing.T) {
	gw := newFakeGateway()
	svc := testService(t, gw, okResolver())

	req := plainRequest("hi")
	req.Message.Metadata = map[string]any{"scope": "attacker", "tenant": "attacker"}
	req.Metadata = map[string]any{"scope": "attacker"}

	if _, err := svc.SendMessage(context.Background(), Credentials{
		Headers: map[string][]string{"X-Scope": {"attacker"}, "X-Tenant": {"attacker"}},
	}, req); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	got := gw.scopeSeen()
	if len(got.Levels) != 1 || got.Levels[0].Value != "resolved" {
		t.Fatalf("scope = %+v, want the resolver's and nothing else", got.Levels)
	}
}

// A peer must not be able to present itself as one of your own agents.
func TestSenderIsNamespacedByThePeersNode(t *testing.T) {
	gw := newFakeGateway()
	svc := testService(t, gw, okResolver())

	req := plainRequest("trust me")
	req.SenderName = "planner" // the name of a local agent, as it happens

	if _, err := svc.SendMessage(context.Background(), Credentials{}, req); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	sender := gw.params().Sender
	if sender.Node != "peer.example" {
		t.Fatalf("sender = %+v, want it namespaced by the peer's node", sender)
	}
	if sender.IsLocal() {
		t.Fatal("a remote peer must never present as a local agent")
	}
}

func TestSendMessageRefusesAnUnauthenticatedCaller(t *testing.T) {
	gw := newFakeGateway()
	svc := testService(t, gw, staticResolver{err: errors.New("no credentials")})

	_, err := svc.SendMessage(context.Background(), Credentials{}, plainRequest("hi"))
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Code != CodeInvalidRequest {
		t.Fatalf("err = %v, want a refusal", err)
	}
	if gw.calls() != 0 {
		t.Fatal("a refused caller reached the gateway")
	}
}

// A resolver that answers with no scope is a resolver that did not
// really answer. Letting that through would run the message with an
// empty scope, which every store call refuses anyway, several layers
// further down and with a stranger error.
func TestAResolverWithNoScopeIsRefused(t *testing.T) {
	gw := newFakeGateway()
	svc := testService(t, gw, staticResolver{peer: Peer{Node: "peer.example"}})

	if _, err := svc.SendMessage(context.Background(), Credentials{}, plainRequest("hi")); err == nil {
		t.Fatal("want a refusal")
	}
	if gw.calls() != 0 {
		t.Fatal("a peer with no scope reached the gateway")
	}
}

// A caller must not be able to enumerate which agents exist by probing
// names, so an unknown tenant is the same answer as an unknown task.
func TestUnknownTenantIsTaskNotFound(t *testing.T) {
	gw := newFakeGateway()
	svc := testService(t, gw, okResolver())

	req := plainRequest("hi")
	req.Tenant = "does-not-exist"

	_, err := svc.SendMessage(context.Background(), Credentials{}, req)
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Code != CodeTaskNotFound {
		t.Fatalf("err = %v, want TaskNotFoundError", err)
	}
	if containsFold(aerr.Message, "agent") {
		t.Fatalf("the refusal tells the caller what kind of thing was missing: %q", aerr.Message)
	}
}

func TestMissingTenantIsInvalidParams(t *testing.T) {
	svc := testService(t, newFakeGateway(), okResolver())
	req := plainRequest("hi")
	req.Tenant = ""

	_, err := svc.SendMessage(context.Background(), Credentials{}, req)
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Code != CodeInvalidParams {
		t.Fatalf("err = %v, want InvalidParams", err)
	}
}

// An informative is filed, not worked on, so there is no task to return.
func TestInformativeReturnsAMessageAcknowledgement(t *testing.T) {
	gw := newFakeGateway()
	svc := testService(t, gw, okResolver())

	req := plainRequest("the build is green")
	req.Message.Metadata = map[string]any{FIPAExtensionURI: map[string]any{"performative": "inform"}}

	res, err := svc.SendMessage(context.Background(), Credentials{}, req)
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if res.Task != nil {
		t.Fatalf("an inform started a task: %+v", res.Task)
	}
	if res.Message == nil || res.Message.ContextID == "" {
		t.Fatalf("an acknowledgement must name the conversation it joined: %+v", res.Message)
	}
}

// A directive is work, so the peer gets a task it can poll and cancel.
func TestDirectiveReturnsATaskBackedByTheRun(t *testing.T) {
	gw := newFakeGateway()
	runID := id.NewAgentRunID()
	convID := id.NewConversationID()
	gw.sendResult = &a2a.SendResult{
		MessageID:      id.NewMessageID(),
		ConversationID: convID,
		Deliveries:     []a2a.DeliveryOutcome{{Receiver: a2a.Address{Agent: "worker"}, Status: a2a.DeliveryQueued}},
	}
	gw.addRun(&run.Run{ID: runID, State: run.StateRunning})
	svc := testService(t, gw, okResolver())

	res, err := svc.SendMessage(context.Background(), Credentials{}, plainRequest("do the thing"))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if res.Task == nil {
		t.Fatal("a request must come back as a task the peer can follow")
	}
	if res.Task.ContextID != convID.String() {
		t.Errorf("contextId = %q, want the conversation id", res.Task.ContextID)
	}
	// The run has not been started by the time the send returns, so the
	// task is submitted rather than working. Claiming WORKING for a run
	// that has not begun would be a small lie with no upside.
	if res.Task.Status.State != TaskStateSubmitted {
		t.Errorf("state = %s, want submitted", res.Task.Status.State)
	}
}

func TestGetTaskProjectsTheRun(t *testing.T) {
	gw := newFakeGateway()
	runID := id.NewAgentRunID()
	gw.addRun(&run.Run{ID: runID, State: run.StateCompleted, Output: "all done"})
	svc := testService(t, gw, okResolver())

	task, err := svc.GetTask(context.Background(), Credentials{}, GetTaskRequest{Tenant: "worker", ID: runID.String()})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status.State != TaskStateCompleted || len(task.Artifacts) != 1 {
		t.Fatalf("task = %+v", task)
	}
}

func TestGetTaskOfSomethingElseIsNotFound(t *testing.T) {
	svc := testService(t, newFakeGateway(), okResolver())

	_, err := svc.GetTask(context.Background(), Credentials{}, GetTaskRequest{Tenant: "worker", ID: id.NewAgentRunID().String()})
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Code != CodeTaskNotFound {
		t.Fatalf("err = %v, want TaskNotFoundError", err)
	}
}

func TestGetTaskRejectsAnIDThatIsNotARunID(t *testing.T) {
	svc := testService(t, newFakeGateway(), okResolver())

	_, err := svc.GetTask(context.Background(), Credentials{}, GetTaskRequest{Tenant: "worker", ID: "not-an-id"})
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Code != CodeTaskNotFound {
		t.Fatalf("err = %v, want TaskNotFoundError rather than a parse error", err)
	}
}

func TestCancelTaskOfATerminalRunIsNotCancelable(t *testing.T) {
	gw := newFakeGateway()
	runID := id.NewAgentRunID()
	gw.addRun(&run.Run{ID: runID, State: run.StateCompleted})
	svc := testService(t, gw, okResolver())

	_, err := svc.CancelTask(context.Background(), Credentials{}, CancelTaskRequest{Tenant: "worker", ID: runID.String()})
	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Code != CodeTaskNotCancelable {
		t.Fatalf("err = %v, want TaskNotCancelableError", err)
	}
}

func TestCancelTaskCancelsARunningRun(t *testing.T) {
	gw := newFakeGateway()
	runID := id.NewAgentRunID()
	gw.addRun(&run.Run{ID: runID, State: run.StateRunning})
	svc := testService(t, gw, okResolver())

	task, err := svc.CancelTask(context.Background(), Credentials{}, CancelTaskRequest{Tenant: "worker", ID: runID.String()})
	if err != nil {
		t.Fatalf("CancelTask: %v", err)
	}
	if task.Status.State != TaskStateCanceled {
		t.Fatalf("state = %s, want canceled", task.Status.State)
	}
}

func TestListTasksProjectsEveryRun(t *testing.T) {
	gw := newFakeGateway()
	gw.addRun(&run.Run{ID: id.NewAgentRunID(), State: run.StateRunning})
	gw.addRun(&run.Run{ID: id.NewAgentRunID(), State: run.StateCompleted})
	svc := testService(t, gw, okResolver())

	res, err := svc.ListTasks(context.Background(), Credentials{}, ListTasksRequest{Tenant: "worker"})
	if err != nil {
		t.Fatalf("ListTasks: %v", err)
	}
	if len(res.Tasks) != 2 {
		t.Fatalf("got %d tasks, want 2", len(res.Tasks))
	}
}

// Every method authenticates. A method that forgot would be an
// unauthenticated read of somebody else's tasks.
func TestEveryMethodRefusesAnUnauthenticatedCaller(t *testing.T) {
	svc := testService(t, newFakeGateway(), staticResolver{err: errors.New("nope")})
	ctx, cred := context.Background(), Credentials{}

	if _, err := svc.SendMessage(ctx, cred, plainRequest("x")); err == nil {
		t.Error("SendMessage let an unauthenticated caller through")
	}
	if _, err := svc.GetTask(ctx, cred, GetTaskRequest{ID: "x"}); err == nil {
		t.Error("GetTask let an unauthenticated caller through")
	}
	if _, err := svc.ListTasks(ctx, cred, ListTasksRequest{}); err == nil {
		t.Error("ListTasks let an unauthenticated caller through")
	}
	if _, err := svc.CancelTask(ctx, cred, CancelTaskRequest{ID: "x"}); err == nil {
		t.Error("CancelTask let an unauthenticated caller through")
	}
}

func TestNewServiceNeedsAGatewayAndAResolver(t *testing.T) {
	if NewService(nil, okResolver(), Options{}) != nil {
		t.Error("a service with no gateway must not build")
	}
	if NewService(newFakeGateway(), nil, Options{}) != nil {
		t.Error("a service with no resolver would be an open door")
	}
	_ = cortex.Scope{}
}

// A peer polls with the handle it was given at send time, which is a
// delivery id, and it must resolve to whatever became of the message.
func TestGetTaskFollowsADeliveryToItsRun(t *testing.T) {
	gw := newFakeGateway()
	runID := id.NewAgentRunID()
	dlvID := id.NewDeliveryID()
	gw.addRun(&run.Run{ID: runID, State: run.StateCompleted, Output: "answered"})
	gw.addDelivery(&a2a.Delivery{ID: dlvID, State: a2a.DeliveryDelivered, RunID: runID})
	svc := testService(t, gw, okResolver())

	task, err := svc.GetTask(context.Background(), Credentials{}, GetTaskRequest{Tenant: "worker", ID: dlvID.String()})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status.State != TaskStateCompleted {
		t.Fatalf("state = %s, want completed", task.Status.State)
	}
	if len(task.Artifacts) != 1 || task.Artifacts[0].Parts[0].Text != "answered" {
		t.Fatalf("artifacts = %+v", task.Artifacts)
	}
}

// A delivery that has not started a run yet is a real state, not a
// missing task. A peer polling right after sending must not be told its
// task does not exist.
func TestGetTaskOfAQueuedDeliveryIsSubmitted(t *testing.T) {
	gw := newFakeGateway()
	dlvID := id.NewDeliveryID()
	gw.addDelivery(&a2a.Delivery{ID: dlvID, State: a2a.DeliveryQueued})
	svc := testService(t, gw, okResolver())

	task, err := svc.GetTask(context.Background(), Credentials{}, GetTaskRequest{Tenant: "worker", ID: dlvID.String()})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status.State != TaskStateSubmitted {
		t.Fatalf("state = %s, want submitted", task.Status.State)
	}
}

func TestGetTaskOfAFailedDeliveryIsFailed(t *testing.T) {
	gw := newFakeGateway()
	dlvID := id.NewDeliveryID()
	gw.addDelivery(&a2a.Delivery{ID: dlvID, State: a2a.DeliveryFailed, Error: "peer unreachable"})
	svc := testService(t, gw, okResolver())

	task, err := svc.GetTask(context.Background(), Credentials{}, GetTaskRequest{Tenant: "worker", ID: dlvID.String()})
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if task.Status.State != TaskStateFailed {
		t.Fatalf("state = %s, want failed", task.Status.State)
	}
}

// The handle a peer is handed must be one it can poll with. A task id
// that resolves to nothing is worse than no task at all.
func TestTheTaskIDFromSendResolves(t *testing.T) {
	gw := newFakeGateway()
	dlvID := id.NewDeliveryID()
	gw.sendResult = &a2a.SendResult{
		MessageID:      id.NewMessageID(),
		ConversationID: id.NewConversationID(),
		Deliveries: []a2a.DeliveryOutcome{{
			Receiver: a2a.Address{Agent: "worker"}, Status: a2a.DeliveryQueued, DeliveryID: dlvID,
		}},
	}
	gw.addDelivery(&a2a.Delivery{ID: dlvID, State: a2a.DeliveryQueued})
	svc := testService(t, gw, okResolver())

	res, err := svc.SendMessage(context.Background(), Credentials{}, plainRequest("do the thing"))
	if err != nil {
		t.Fatalf("SendMessage: %v", err)
	}
	if res.Task == nil {
		t.Fatal("a request must come back as a task")
	}
	if _, err := svc.GetTask(context.Background(), Credentials{}, GetTaskRequest{
		Tenant: "worker", ID: res.Task.ID,
	}); err != nil {
		t.Fatalf("the id handed back by SendMessage does not resolve: %v", err)
	}
}
