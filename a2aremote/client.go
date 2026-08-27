package a2aremote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/xraph/cortex/a2a"
)

// Defaults for a client.
const (
	DefaultPollInterval = 2 * time.Second
	DefaultPollTimeout  = 2 * time.Minute
)

// PeerConfig is one remote agent host this engine will talk to.
//
// Peers are configuration rather than data on purpose. An agent's own
// output cannot introduce one, so a prompt-injected agent can at worst
// misuse a peer that was already trusted.
type PeerConfig struct {
	// Node is the address suffix agents write: worker@<node>.
	Node string
	// BaseURL is where the peer's agent card and endpoint live.
	BaseURL string
	// Header carries whatever the peer authenticates on.
	Header http.Header
}

// ReplySink is how an answer re-enters cortex. The engine wires it to
// Bus.Send, so a remote reply travels the same path a local one does and
// resolves a waiting ask the same way.
type ReplySink func(ctx context.Context, p a2a.SendParams) error

// ClientOptions tunes the outbound client.
type ClientOptions struct {
	HTTPClient   *http.Client
	PollInterval time.Duration
	PollTimeout  time.Duration
}

// Client carries cortex envelopes to remote A2A agents. It satisfies
// a2a.Transport, which is the seam the bus already routes through.
type Client struct {
	peers  map[string]PeerConfig
	sink   ReplySink
	http   *http.Client
	poll   time.Duration
	maxAge time.Duration

	mu      sync.Mutex
	cards   map[string]AgentCard
	clients map[string]*http.Client
}

// NewClient builds a client over the given peers.
func NewClient(peers []PeerConfig, sink ReplySink, opts ClientOptions) *Client {
	c := &Client{
		peers:   make(map[string]PeerConfig, len(peers)),
		sink:    sink,
		http:    opts.HTTPClient,
		poll:    opts.PollInterval,
		maxAge:  opts.PollTimeout,
		cards:   map[string]AgentCard{},
		clients: map[string]*http.Client{},
	}
	for _, p := range peers {
		c.peers[p.Node] = p
	}
	if c.http == nil {
		c.http = http.DefaultClient
	}
	if c.poll <= 0 {
		c.poll = DefaultPollInterval
	}
	if c.maxAge <= 0 {
		c.maxAge = DefaultPollTimeout
	}
	return c
}

// Handles claims addresses at registered peers, and nothing else.
//
// This is the outbound half of the trust boundary: an agent that names a
// hostname nobody configured gets an unroutable error rather than a
// connection.
func (c *Client) Handles(addr a2a.Address) bool {
	if addr.IsLocal() {
		return false
	}
	_, ok := c.peers[addr.Node]
	return ok
}

// Deliver carries one envelope to a remote agent and feeds the answer
// back through the sink.
//
// A message with no ReplyWith is fire-and-forget: it is delivered and
// nothing comes back, because nobody is waiting for anything.
func (c *Client) Deliver(ctx context.Context, e *a2a.Envelope, receiver a2a.Address) error {
	peer, ok := c.peers[receiver.Node]
	if !ok {
		return fmt.Errorf("no peer configured for %q", receiver.Node)
	}

	endpoint, tenant, err := c.endpointFor(ctx, peer, receiver)
	if err != nil {
		return err
	}

	res, err := c.sendMessage(ctx, peer, endpoint, SendMessageRequest{
		Tenant:     tenant,
		Message:    MessageFromEnvelope(e),
		SenderName: e.Sender.Agent,
	})
	if err != nil {
		return err
	}

	// Nobody is waiting, so there is nothing to carry home.
	if e.ReplyWith == "" {
		return nil
	}

	answer, err := c.answerOf(ctx, peer, endpoint, tenant, res, e)
	if err != nil {
		return err
	}
	if c.sink == nil {
		return nil
	}
	return c.sink(ctx, a2a.SendParams{
		Sender:         receiver,
		Receivers:      []a2a.Address{e.Sender},
		Performative:   a2a.Inform,
		Content:        answer,
		ConversationID: e.ConversationID,
		InReplyTo:      e.ReplyWith,
		Protocol:       e.Protocol,
		Ontology:       e.Ontology,
	})
}

// answerOf turns a peer's response into the text the waiting agent gets.
// A task that has not finished is polled, bounded by the ask's own
// deadline, which is already the thing that turns silence into a
// readable failure.
func (c *Client) answerOf(ctx context.Context, peer PeerConfig, endpoint, tenant string, res *SendMessageResult, e *a2a.Envelope) (string, error) {
	switch {
	case res.Message != nil:
		return textOf(res.Message.Parts), nil
	case res.Task == nil:
		return "", ErrInvalidAgentResponse("the peer returned neither a message nor a task")
	}

	deadline := time.Now().Add(c.maxAge)
	if e.ReplyBy != nil && e.ReplyBy.Before(deadline) {
		deadline = *e.ReplyBy
	}

	task := res.Task
	for {
		if task.Status.State.Terminal() {
			return answerFromTask(task)
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("%s did not finish before the deadline", task.ID)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(c.poll):
		}

		polled, err := c.getTask(ctx, peer, endpoint, GetTaskRequest{Tenant: tenant, ID: task.ID})
		if err != nil {
			return "", err
		}
		task = polled
	}
}

func answerFromTask(task *Task) (string, error) {
	switch task.Status.State {
	case TaskStateCompleted:
		parts := make([]Part, 0, len(task.Artifacts))
		for _, a := range task.Artifacts {
			parts = append(parts, a.Parts...)
		}
		if text := textOf(parts); text != "" {
			return text, nil
		}
		if task.Status.Message != nil {
			return textOf(task.Status.Message.Parts), nil
		}
		return "", ErrInvalidAgentResponse("the peer completed the task with no output")
	default:
		// Failed, canceled or rejected. The peer's own words are more
		// useful to the waiting agent than a status name.
		if task.Status.Message != nil {
			if text := textOf(task.Status.Message.Parts); text != "" {
				return "", fmt.Errorf("the peer could not answer: %s", text)
			}
		}
		return "", fmt.Errorf("the peer ended the task as %s", task.Status.State)
	}
}

// endpointFor reads the peer's card and picks a binding we speak.
func (c *Client) endpointFor(ctx context.Context, peer PeerConfig, receiver a2a.Address) (endpoint, tenant string, err error) {
	card, err := c.card(ctx, peer)
	if err != nil {
		return "", "", err
	}
	for _, iface := range card.SupportedInterfaces {
		if iface.ProtocolBinding != BindingJSONRPC {
			continue
		}
		// The tenant the card declares wins over the agent name we were
		// addressed with: the peer decides how it routes internally.
		t := iface.Tenant
		if t == "" {
			t = receiver.Agent
		}
		return iface.URL, t, nil
	}
	return "", "", fmt.Errorf("%s speaks no binding this client supports", peer.Node)
}

// card reads a peer's agent card, once per peer.
func (c *Client) card(ctx context.Context, peer PeerConfig) (AgentCard, error) {
	c.mu.Lock()
	cached, ok := c.cards[peer.Node]
	c.mu.Unlock()
	if ok {
		return cached, nil
	}

	card, err := FetchCard(ctx, c.clientFor(peer), peer.BaseURL)
	if err != nil {
		return AgentCard{}, err
	}
	c.mu.Lock()
	c.cards[peer.Node] = card
	c.mu.Unlock()
	return card, nil
}

// clientFor returns an HTTP client that presents this peer's
// credentials on every request.
//
// It wraps rather than setting headers per call, because the card fetch
// needs them too: a peer that gates its agent card behind credentials
// would otherwise be undiscoverable, and the failure would look like a
// missing card rather than a missing token.
func (c *Client) clientFor(peer PeerConfig) *http.Client {
	if len(peer.Header) == 0 {
		return c.http
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.clients[peer.Node]; ok {
		return existing
	}

	base := c.http.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	wrapped := &http.Client{
		Timeout:   c.http.Timeout,
		Transport: headerTransport{base: base, header: peer.Header.Clone()},
	}
	c.clients[peer.Node] = wrapped
	return wrapped
}

// headerTransport adds a fixed set of headers to every request.
type headerTransport struct {
	base   http.RoundTripper
	header http.Header
}

func (t headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	// The request is cloned rather than mutated: a RoundTripper must not
	// modify the request it was handed.
	clone := r.Clone(r.Context())
	for k, vs := range t.header {
		for _, v := range vs {
			clone.Header.Add(k, v)
		}
	}
	return t.base.RoundTrip(clone)
}

func (c *Client) sendMessage(ctx context.Context, peer PeerConfig, endpoint string, req SendMessageRequest) (*SendMessageResult, error) {
	var out SendMessageResult
	if err := c.rpc(ctx, peer, endpoint, MethodSendMessage, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) getTask(ctx context.Context, peer PeerConfig, endpoint string, req GetTaskRequest) (*Task, error) {
	var out Task
	if err := c.rpc(ctx, peer, endpoint, MethodGetTask, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// rpc performs one JSON-RPC call against a peer.
func (c *Client) rpc(ctx context.Context, peer PeerConfig, endpoint, method string, params, dest any) error {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: method, Params: mustRaw(params)})
	if err != nil {
		return fmt.Errorf("encode %s: %w", method, err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build %s request: %w", method, err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(versionHeader, ProtocolVersion)

	resp, err := c.clientFor(peer).Do(httpReq)
	if err != nil {
		return fmt.Errorf("call %s at %s: %w", method, peer.Node, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s at %s returned %s", method, peer.Node, resp.Status)
	}

	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  *Error          `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRequestBytes)).Decode(&envelope); err != nil {
		return ErrInvalidAgentResponse("the peer's response was not JSON-RPC")
	}
	if envelope.Error != nil {
		return envelope.Error
	}
	if len(envelope.Result) == 0 {
		return ErrInvalidAgentResponse("the peer answered with no result")
	}
	if err := json.Unmarshal(envelope.Result, dest); err != nil {
		return ErrInvalidAgentResponse("the peer's result did not decode: " + err.Error())
	}
	return nil
}

func mustRaw(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}

func textOf(parts []Part) string {
	var texts []string
	for _, p := range parts {
		if p.Text != "" {
			texts = append(texts, p.Text)
		}
	}
	return strings.Join(texts, "\n\n")
}

// Compile-time proof that a client is a transport the bus can use.
var _ a2a.Transport = (*Client)(nil)
