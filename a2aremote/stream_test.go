package a2aremote

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func streamingService(t *testing.T, gw Gateway) *Service {
	t.Helper()
	return NewService(gw, okResolver(), Options{Streaming: true, StreamPoll: time.Millisecond})
}

// movingGateway walks a run through its states on successive reads, so a
// subscription sees transitions rather than a single snapshot.
type movingGateway struct {
	*fakeGateway
	mu     sync.Mutex
	states []run.State
	runID  id.AgentRunID
}

func newMovingGateway(states ...run.State) *movingGateway {
	g := &movingGateway{fakeGateway: newFakeGateway(), states: states, runID: id.NewAgentRunID()}
	dlvID := id.NewDeliveryID()
	g.addDelivery(&a2a.Delivery{ID: dlvID, State: a2a.DeliveryDelivered, RunID: g.runID})
	g.sendResult = &a2a.SendResult{
		MessageID:      id.NewMessageID(),
		ConversationID: id.NewConversationID(),
		Deliveries:     []a2a.DeliveryOutcome{{Receiver: a2a.Address{Agent: "worker"}, DeliveryID: dlvID}},
	}
	return g
}

func (g *movingGateway) GetRun(_ context.Context, runID id.AgentRunID) (*run.Run, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if runID != g.runID {
		return nil, errors.New("not found")
	}
	state := g.states[0]
	if len(g.states) > 1 {
		g.states = g.states[1:]
	}
	r := &run.Run{ID: g.runID, State: state}
	if state == run.StateCompleted {
		r.Output = "the work is done"
	}
	return r, nil
}

func collect(t *testing.T, gw Gateway, run func(*Service, Emit) error) []StreamEvent {
	t.Helper()
	var events []StreamEvent
	if err := run(streamingService(t, gw), func(ev StreamEvent) error {
		events = append(events, ev)
		return nil
	}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	return events
}

// The opening event is the task as it exists right now, so a client has
// a handle immediately rather than after the first transition.
func TestStreamMessageOpensWithTheTask(t *testing.T) {
	gw := newMovingGateway(run.StateRunning, run.StateCompleted)
	events := collect(t, gw, func(s *Service, emit Emit) error {
		return s.StreamMessage(context.Background(), Credentials{}, plainRequest("do the thing"), emit)
	})

	if len(events) == 0 || events[0].Task == nil {
		t.Fatalf("the first event must be the task: %+v", events)
	}
}

func TestStreamMessageReportsTransitionsAndEndsFinal(t *testing.T) {
	gw := newMovingGateway(run.StateRunning, run.StateRunning, run.StateCompleted)
	events := collect(t, gw, func(s *Service, emit Emit) error {
		return s.StreamMessage(context.Background(), Credentials{}, plainRequest("do the thing"), emit)
	})

	var updates []*TaskStatusUpdateEvent
	var artifacts []*TaskArtifactUpdate
	for _, ev := range events {
		switch {
		case ev.StatusUpdate != nil:
			updates = append(updates, ev.StatusUpdate)
		case ev.ArtifactUpdate != nil:
			artifacts = append(artifacts, ev.ArtifactUpdate)
		}
	}

	if len(updates) == 0 {
		t.Fatalf("no transitions were reported: %+v", events)
	}
	last := updates[len(updates)-1]
	if !last.Final {
		t.Error("the last transition must say it is final, or a client keeps a dead connection open")
	}
	if last.Status.State != TaskStateCompleted {
		t.Errorf("last state = %s, want completed", last.Status.State)
	}
	// The output arrives with the last transition rather than after it,
	// so a client that stops on Final still has everything.
	if len(artifacts) != 1 || artifacts[0].Artifact.Parts[0].Text != "the work is done" {
		t.Fatalf("artifacts = %+v", artifacts)
	}
}

// The same state read twice is not a transition, and reporting it would
// fill a client's stream with noise.
func TestStreamDoesNotRepeatAState(t *testing.T) {
	gw := newMovingGateway(run.StateRunning, run.StateRunning, run.StateRunning, run.StateCompleted)
	events := collect(t, gw, func(s *Service, emit Emit) error {
		return s.StreamMessage(context.Background(), Credentials{}, plainRequest("go"), emit)
	})

	var working int
	for _, ev := range events {
		if ev.StatusUpdate != nil && ev.StatusUpdate.Status.State == TaskStateWorking {
			working++
		}
	}
	if working > 1 {
		t.Fatalf("the same state was reported %d times", working)
	}
}

// An informative starts no work, so the acknowledgement is the whole
// stream. Holding a connection open for it would be a subscription to
// nothing.
func TestStreamOfAnInformativeIsJustTheAcknowledgement(t *testing.T) {
	gw := newFakeGateway()
	req := plainRequest("the build is green")
	req.Message.Metadata = map[string]any{FIPAExtensionURI: map[string]any{"performative": "inform"}}

	events := collect(t, gw, func(s *Service, emit Emit) error {
		return s.StreamMessage(context.Background(), Credentials{}, req, emit)
	})
	if len(events) != 1 || events[0].Message == nil {
		t.Fatalf("events = %+v, want one acknowledgement", events)
	}
}

// Subscribing to something already finished is an unsupported operation
// rather than an empty stream, which is what the protocol says too.
func TestSubscribeToATerminalTaskIsRefused(t *testing.T) {
	gw := newFakeGateway()
	runID := id.NewAgentRunID()
	gw.addRun(&run.Run{ID: runID, State: run.StateCompleted, Output: "already done"})

	err := streamingService(t, gw).SubscribeTask(context.Background(), Credentials{}, "worker", runID.String(),
		func(StreamEvent) error { return nil })

	var aerr *Error
	if !errors.As(err, &aerr) || aerr.Code != CodeUnsupportedOperation {
		t.Fatalf("err = %v, want UnsupportedOperationError", err)
	}
}

// A client that goes away ends its own subscription and nothing else.
func TestStreamStopsWhenTheClientLeaves(t *testing.T) {
	gw := newMovingGateway(run.StateRunning)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- streamingService(t, gw).StreamMessage(ctx, Credentials{}, plainRequest("go"), func(StreamEvent) error {
			return nil
		})
	}()

	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want the cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream kept running after its client left")
	}
}

func TestStreamingIsOffUnlessAskedFor(t *testing.T) {
	gw := newFakeGateway()
	// The default service has streaming off, and its card says so.
	svc := NewService(gw, okResolver(), Options{Card: CardOptions{BaseURL: "https://x/a2a"}, Exposed: []string{"worker"}})

	rec := httptest.NewRecorder()
	svc.JSONRPCHandler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/", bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":1,"method":"SendStreamingMessage","params":{"tenant":"worker"}}`)))

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body: %q", rec.Body.String())
	}
	e, _ := resp["error"].(map[string]any)
	if e == nil || e["code"] != float64(CodeUnsupportedOperation) {
		t.Fatalf("resp = %+v, want an unsupported-operation refusal", resp)
	}
}

// The transport has to actually stream: frames arrive as they happen
// rather than in one lump when the handler returns.
func TestSSEFramesArriveAsEvents(t *testing.T) {
	gw := newMovingGateway(run.StateRunning, run.StateCompleted)
	srv := httptest.NewServer(streamingService(t, gw).RESTHandler())
	defer srv.Close()

	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost,
		srv.URL+"/worker/message:stream",
		bytes.NewBufferString(`{"message":{"messageId":"m1","role":"ROLE_USER","parts":[{"text":"go"}]}}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content type = %q, want an event stream", ct)
	}

	var frames []StreamEvent
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev StreamEvent
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &ev); err != nil {
			t.Fatalf("frame is not an event: %q", line)
		}
		frames = append(frames, ev)
	}

	if len(frames) < 2 {
		t.Fatalf("got %d frames, want the task and at least one transition", len(frames))
	}
	if frames[0].Task == nil {
		t.Errorf("the first frame must be the task: %+v", frames[0])
	}
	var sawFinal bool
	for _, f := range frames {
		if f.StatusUpdate != nil && f.StatusUpdate.Final {
			sawFinal = true
		}
	}
	if !sawFinal {
		t.Error("the stream never said it was finished")
	}
}
