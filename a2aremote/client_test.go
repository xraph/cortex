package a2aremote

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
)

// peerServer is a stand-in for a remote A2A agent: it serves a card and
// answers SendMessage and GetTask however the test tells it to.
type peerServer struct {
	*httptest.Server
	mu       sync.Mutex
	cardHits int
	sends    []SendMessageRequest
	reply    func(SendMessageRequest) (any, *Error)
	getTask  func(GetTaskRequest) (any, *Error)
}

func newPeerServer(t *testing.T) *peerServer {
	t.Helper()
	p := &peerServer{}
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+WellKnownCardPath, func(w http.ResponseWriter, _ *http.Request) {
		p.mu.Lock()
		p.cardHits++
		p.mu.Unlock()
		card := BuildCard(&agent.Config{Name: "specialist", Description: "answers things"}, nil, CardOptions{
			BaseURL: p.URL + "/rpc",
		})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
	})

	mux.HandleFunc("POST /rpc", func(w http.ResponseWriter, r *http.Request) {
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)

		var result any
		var rpcErr *Error
		switch req.Method {
		case MethodSendMessage:
			var sm SendMessageRequest
			_ = json.Unmarshal(req.Params, &sm)
			p.mu.Lock()
			p.sends = append(p.sends, sm)
			p.mu.Unlock()
			if p.reply != nil {
				result, rpcErr = p.reply(sm)
			} else {
				result = SendMessageResult{Message: &Message{
					MessageID: "m-reply", Role: RoleAgent, Parts: []Part{{Text: "an answer"}},
				}}
			}
		case MethodGetTask:
			var gt GetTaskRequest
			_ = json.Unmarshal(req.Params, &gt)
			if p.getTask != nil {
				result, rpcErr = p.getTask(gt)
			} else {
				rpcErr = ErrTaskNotFound(gt.ID)
			}
		default:
			rpcErr = ErrMethodNotFound(req.Method)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: result, Error: rpcErr})
	})

	p.Server = httptest.NewServer(mux)
	t.Cleanup(p.Close)
	return p
}

func (p *peerServer) lastSend(t *testing.T) SendMessageRequest {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.sends) == 0 {
		t.Fatal("the peer was never called")
	}
	return p.sends[len(p.sends)-1]
}

type recordingSink struct {
	mu     sync.Mutex
	params []a2a.SendParams
}

func (s *recordingSink) fn() ReplySink {
	return func(_ context.Context, p a2a.SendParams) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.params = append(s.params, p)
		return nil
	}
}

func (s *recordingSink) last(t *testing.T) a2a.SendParams {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.params) == 0 {
		t.Fatal("nothing was fed back into the bus")
	}
	return s.params[len(s.params)-1]
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.params)
}

func testEnvelope(replyWith string) *a2a.Envelope {
	return &a2a.Envelope{
		ID: id.NewMessageID(), ConversationID: id.NewConversationID(),
		Performative: a2a.Request, Sender: a2a.Address{Agent: "planner"},
		Receivers: []a2a.Address{{Agent: "specialist", Node: "peer-b"}},
		Content:   "what is the status?", ReplyWith: replyWith,
	}
}

// An agent must not be able to make cortex call a host nobody
// configured. This is the outbound half of the trust boundary.
func TestClientHandlesOnlyRegisteredPeers(t *testing.T) {
	c := NewClient([]PeerConfig{{Node: "peer-b", BaseURL: "https://b.example"}}, nil, ClientOptions{})

	if !c.Handles(a2a.Address{Agent: "x", Node: "peer-b"}) {
		t.Error("a configured peer must be handled")
	}
	if c.Handles(a2a.Address{Agent: "x", Node: "evil.example"}) {
		t.Error("an unconfigured hostname must not be handled")
	}
	if c.Handles(a2a.Address{Agent: "x"}) {
		t.Error("a local address is not this transport's business")
	}
}

func TestDeliverSendsAndFeedsTheReplyBack(t *testing.T) {
	peer := newPeerServer(t)
	sink := &recordingSink{}
	c := NewClient([]PeerConfig{{Node: "peer-b", BaseURL: peer.URL}}, sink.fn(), ClientOptions{})

	e := testEnvelope("rw-1")
	if err := c.Deliver(context.Background(), e, a2a.Address{Agent: "specialist", Node: "peer-b"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	sent := peer.lastSend(t)
	if sent.Tenant != "specialist" {
		t.Errorf("tenant = %q, want the agent name the card declared", sent.Tenant)
	}
	if sent.SenderName != "planner" {
		t.Errorf("senderName = %q, want the local agent that spoke", sent.SenderName)
	}
	if len(sent.Message.Extensions) != 1 || sent.Message.Extensions[0] != FIPAExtensionURI {
		t.Errorf("the outbound message did not declare the extension: %+v", sent.Message.Extensions)
	}

	back := sink.last(t)
	if back.Content != "an answer" {
		t.Errorf("content = %q", back.Content)
	}
	if back.InReplyTo != "rw-1" {
		t.Errorf("inReplyTo = %q, want the token the ask is waiting on", back.InReplyTo)
	}
	if back.ConversationID != e.ConversationID {
		t.Errorf("the reply joined a different conversation")
	}
	if back.Sender.Node != "peer-b" {
		t.Errorf("sender = %+v, want the remote peer", back.Sender)
	}
}

// A send that nobody is waiting on must not manufacture a reply.
func TestFireAndForgetDeliversNothingBack(t *testing.T) {
	peer := newPeerServer(t)
	sink := &recordingSink{}
	c := NewClient([]PeerConfig{{Node: "peer-b", BaseURL: peer.URL}}, sink.fn(), ClientOptions{})

	if err := c.Deliver(context.Background(), testEnvelope(""), a2a.Address{Agent: "specialist", Node: "peer-b"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if sink.count() != 0 {
		t.Fatalf("%d replies fed back for a fire-and-forget send", sink.count())
	}
}

func TestDeliverPollsANonTerminalTask(t *testing.T) {
	peer := newPeerServer(t)
	var polls int
	peer.reply = func(SendMessageRequest) (any, *Error) {
		return SendMessageResult{Task: &Task{ID: "dlv_1", Status: TaskStatus{State: TaskStateWorking}}}, nil
	}
	peer.getTask = func(GetTaskRequest) (any, *Error) {
		polls++
		if polls < 2 {
			return Task{ID: "dlv_1", Status: TaskStatus{State: TaskStateWorking}}, nil
		}
		return Task{
			ID:        "dlv_1",
			Status:    TaskStatus{State: TaskStateCompleted},
			Artifacts: []Artifact{{ArtifactID: "a", Parts: []Part{{Text: "took a while, but done"}}}},
		}, nil
	}

	sink := &recordingSink{}
	c := NewClient([]PeerConfig{{Node: "peer-b", BaseURL: peer.URL}}, sink.fn(),
		ClientOptions{PollInterval: time.Millisecond})

	if err := c.Deliver(context.Background(), testEnvelope("rw-1"), a2a.Address{Agent: "specialist", Node: "peer-b"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if got := sink.last(t).Content; got != "took a while, but done" {
		t.Fatalf("content = %q", got)
	}
}

func TestDeliverSurfacesAPeerError(t *testing.T) {
	peer := newPeerServer(t)
	peer.reply = func(SendMessageRequest) (any, *Error) { return nil, ErrTaskNotFound("worker") }

	sink := &recordingSink{}
	c := NewClient([]PeerConfig{{Node: "peer-b", BaseURL: peer.URL}}, sink.fn(), ClientOptions{})

	err := c.Deliver(context.Background(), testEnvelope("rw-1"), a2a.Address{Agent: "specialist", Node: "peer-b"})
	if err == nil {
		t.Fatal("a peer error must surface, so the bus can fail the ask with it")
	}
	if sink.count() != 0 {
		t.Fatal("a failed delivery must not feed a reply back")
	}
}

func TestDeliverToAnUnconfiguredPeerFails(t *testing.T) {
	c := NewClient(nil, nil, ClientOptions{})
	if err := c.Deliver(context.Background(), testEnvelope("rw-1"), a2a.Address{Agent: "x", Node: "nowhere"}); err == nil {
		t.Fatal("want an error for a peer that was never configured")
	}
}

func TestClientCachesTheAgentCard(t *testing.T) {
	peer := newPeerServer(t)
	c := NewClient([]PeerConfig{{Node: "peer-b", BaseURL: peer.URL}}, (&recordingSink{}).fn(), ClientOptions{})

	for range 3 {
		if err := c.Deliver(context.Background(), testEnvelope("rw-1"), a2a.Address{Agent: "specialist", Node: "peer-b"}); err != nil {
			t.Fatalf("Deliver: %v", err)
		}
	}
	peer.mu.Lock()
	hits := peer.cardHits
	peer.mu.Unlock()
	if hits != 1 {
		t.Fatalf("the card was fetched %d times, want 1", hits)
	}
}

// Whatever a peer authenticates on has to reach it, or every call is
// refused and nothing says why.
func TestClientPresentsItsCredentials(t *testing.T) {
	var (
		mu       sync.Mutex
		seenRPC  string
		seenCard string
	)
	var srv *httptest.Server
	mux := http.NewServeMux()

	mux.HandleFunc("GET "+WellKnownCardPath, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenCard = r.Header.Get("Authorization")
		mu.Unlock()
		card := BuildCard(&agent.Config{Name: "specialist"}, nil, CardOptions{BaseURL: srv.URL + "/rpc"})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(card)
	})
	mux.HandleFunc("POST /rpc", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenRPC = r.Header.Get("Authorization")
		mu.Unlock()
		var req rpcRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: SendMessageResult{
			Message: &Message{MessageID: "m", Role: RoleAgent, Parts: []Part{{Text: "ok"}}},
		}})
	})

	srv = httptest.NewServer(mux)
	defer srv.Close()

	c := NewClient([]PeerConfig{{
		Node: "peer-b", BaseURL: srv.URL,
		Header: http.Header{"Authorization": {"Bearer sekrit"}},
	}}, (&recordingSink{}).fn(), ClientOptions{})

	if err := c.Deliver(context.Background(), testEnvelope("rw-1"), a2a.Address{Agent: "specialist", Node: "peer-b"}); err != nil {
		t.Fatalf("Deliver: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if seenRPC != "Bearer sekrit" {
		t.Errorf("the RPC carried %q, want the peer's credentials", seenRPC)
	}
	// The card fetch carries them too. A peer that gates its card behind
	// credentials would otherwise be undiscoverable, and the failure
	// would read as a missing card rather than a missing token.
	if seenCard != "Bearer sekrit" {
		t.Errorf("the card request carried %q, want the peer's credentials", seenCard)
	}
}
