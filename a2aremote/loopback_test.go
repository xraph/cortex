package a2aremote_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/a2aremote"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
)

// scriptedLLM answers as whichever agent is calling, keyed on the system
// prompt, which is the only thing in a request that says who is asking.
type scriptedLLM struct {
	mu      sync.Mutex
	asked   bool
	answer  string
	resumed chan struct{}
	once    sync.Once
}

func (l *scriptedLLM) Complete(_ context.Context, req *llm.Request) (*llm.Response, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	system := req.System
	for _, m := range req.Messages {
		if m.Role == "system" && system == "" {
			system = m.Content
		}
	}

	if strings.Contains(system, "specialist") {
		return &llm.Response{Content: l.answer}, nil
	}
	if !l.asked {
		l.asked = true
		return &llm.Response{ToolCalls: []llm.ToolCall{{
			ID:        "call-1",
			Name:      "agent_ask",
			Arguments: `{"to":"specialist@peer-b","content":"is the migration safe?"}`,
		}}}, nil
	}
	if l.resumed != nil {
		l.once.Do(func() { close(l.resumed) })
	}
	return &llm.Response{Content: "the specialist has answered"}, nil
}

func (l *scriptedLLM) CompleteStream(context.Context, *llm.Request) (llm.Stream, error) {
	return nil, errors.New("not supported")
}

func newEngine(ctx context.Context, t *testing.T, name string, model llm.Client, agents ...string) (*engine.Engine, *sqlitestore.Store) {
	t.Helper()
	// The busy timeout matters here rather than being test hygiene. The
	// messaging dispatcher writes on its own goroutines while a run is
	// writing on yours, and sqlite answers a concurrent writer with
	// SQLITE_BUSY unless it is told to wait. Any real sqlite deployment
	// of messaging needs this too, and the docs say so.
	dsn := filepath.Join(t.TempDir(), name+".db") + "?_pragma=busy_timeout(5000)"
	drv := sqlitedriver.New()
	if err := drv.Open(ctx, dsn); err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatalf("grove open: %v", err)
	}
	st := sqlitestore.New(db)
	if migrateErr := st.Migrate(ctx); migrateErr != nil {
		t.Fatalf("migrate: %v", migrateErr)
	}
	t.Cleanup(func() { _ = st.Close() })

	eng, err := engine.New(
		engine.WithStore(st),
		engine.WithLLM(model),
		engine.WithA2A(a2a.Options{HopCeiling: 6, Workers: 1}),
	)
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	for _, a := range agents {
		if createErr := eng.CreateAgent(ctx, &agent.Config{
			ID: id.NewAgentID(), Name: a, SystemPrompt: "you are the " + a,
			Model: "test-model", MaxSteps: 4,
		}); createErr != nil {
			t.Fatalf("CreateAgent %s: %v", a, createErr)
		}
	}
	return eng, st
}

// TestTwoEnginesTalkOverTheWire is the whole feature, end to end and
// across a network boundary.
//
// Engine B serves an agent over JSON-RPC. An agent on engine A asks it a
// question, A's run suspends on a row in A's own database, the question
// travels as an A2A message, B runs its agent, the answer comes back,
// and A resumes with it.
//
// Every piece under this is unit tested. What is not, and cannot be, is
// that the pieces line up: the ids, the extension metadata, the sender
// namespacing, the reply correlation and the resume.
func TestTwoEnginesTalkOverTheWire(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "tenant", Value: "acme"}},
	})

	// Engine B, the peer being called.
	engB, _ := newEngine(ctx, t, "b", &scriptedLLM{answer: "yes, the migration is safe"}, "specialist")
	if err := engB.Start(ctx); err != nil {
		t.Fatalf("engine B start: %v", err)
	}
	defer func() { _ = engB.Stop(ctx) }()

	// B authenticates its callers. The resolver is the only thing that
	// decides what scope an inbound message acts in.
	svcB, err := a2aremote.Attach(engB, a2aremote.AttachOptions{
		Resolver: a2aremote.ResolverFunc(func(_ context.Context, cred a2aremote.Credentials) (a2aremote.Peer, error) {
			if cred.Header("authorization") != "Bearer trust-me" {
				return a2aremote.Peer{}, errors.New("who are you")
			}
			return a2aremote.Peer{
				Node:  "peer-a",
				Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "acme"}}},
			}, nil
		}),
		Service: a2aremote.Options{
			Exposed:      []string{"specialist"},
			DefaultAgent: "specialist",
			Scope:        cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "acme"}}},
		},
	})
	if err != nil {
		t.Fatalf("attach B: %v", err)
	}

	srv := httptest.NewServer(svcB.Handler())
	defer srv.Close()

	// The card has to point back at the server it is served from, which
	// the host knows and cortex does not.
	svcB, err = a2aremote.Attach(engB, a2aremote.AttachOptions{
		Resolver: a2aremote.ResolverFunc(func(_ context.Context, cred a2aremote.Credentials) (a2aremote.Peer, error) {
			if cred.Header("authorization") != "Bearer trust-me" {
				return a2aremote.Peer{}, errors.New("who are you")
			}
			return a2aremote.Peer{
				Node:  "peer-a",
				Scope: cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "acme"}}},
			}, nil
		}),
		Service: a2aremote.Options{
			Card:         a2aremote.CardOptions{BaseURL: srv.URL, Version: "1.0.0"},
			Exposed:      []string{"specialist"},
			DefaultAgent: "specialist",
			Scope:        cortex.Scope{Levels: []cortex.Level{{Key: "tenant", Value: "acme"}}},
		},
	})
	if err != nil {
		t.Fatalf("re-attach B: %v", err)
	}
	srv.Config.Handler = svcB.Handler()

	// Engine A, the caller.
	modelA := &scriptedLLM{resumed: make(chan struct{})}
	engA, stA := newEngine(ctx, t, "a", modelA, "planner")
	if _, attachErr := a2aremote.Attach(engA, a2aremote.AttachOptions{
		Resolver: a2aremote.ResolverFunc(func(context.Context, a2aremote.Credentials) (a2aremote.Peer, error) {
			return a2aremote.Peer{}, errors.New("A takes no inbound calls in this test")
		}),
		Peers: []a2aremote.PeerConfig{{
			Node:    "peer-b",
			BaseURL: srv.URL,
			Header:  headerWith("Authorization", "Bearer trust-me"),
		}},
		Client: a2aremote.ClientOptions{PollInterval: 10 * time.Millisecond},
	}); attachErr != nil {
		t.Fatalf("attach A: %v", attachErr)
	}
	if startErr := engA.Start(ctx); startErr != nil {
		t.Fatalf("engine A start: %v", startErr)
	}
	defer func() { _ = engA.Stop(ctx) }()

	// The planner asks the specialist, over the wire.
	paused, runErr := engA.RunAgent(ctx, "planner", "find out whether tonight's migration is safe", nil)
	if runErr != nil {
		t.Fatalf("RunAgent: %v", runErr)
	}
	if paused.State != run.StatePaused {
		t.Fatalf("state = %s, want paused while the peer answers", paused.State)
	}

	select {
	case <-modelA.resumed:
	case <-time.After(15 * time.Second):
		final, _ := stA.GetRun(ctx, paused.ID)
		t.Fatalf("the planner never resumed; the run is %s", final.State)
	}

	// The model's signal fires while the resumed run is still finishing,
	// so the run's own terminal state is what to wait on.
	final := waitForTerminal(ctx, t, stA, paused.ID)
	if final.State != run.StateCompleted {
		t.Fatalf("state = %s, want completed (error: %q)", final.State, final.Error)
	}

	// The answer reached the model as the ask's tool result.
	steps, err := stA.ListSteps(ctx, paused.ID)
	if err != nil {
		t.Fatalf("ListSteps: %v", err)
	}
	var carried bool
	for _, s := range steps {
		calls, callErr := stA.ListToolCalls(ctx, s.ID)
		if callErr != nil {
			t.Fatalf("ListToolCalls: %v", callErr)
		}
		for _, c := range calls {
			if strings.Contains(c.Result, "yes, the migration is safe") {
				carried = true
			}
		}
	}
	if !carried {
		t.Fatal("the peer's answer never reached the asking model")
	}

	// The peer is recorded as remote, not as a local agent that happens
	// to share a name.
	msgs, err := stA.ListMessages(ctx, &a2a.MessageListFilter{Limit: 10})
	if err != nil {
		t.Fatalf("ListMessages: %v", err)
	}
	var sawRemoteSender bool
	for _, m := range msgs {
		if m.Sender.Node == "peer-b" {
			sawRemoteSender = true
		}
	}
	if !sawRemoteSender {
		t.Fatal("the reply was not recorded as coming from the remote peer")
	}
}

// waitForTerminal polls a run until it stops moving. Polling is the
// honest thing here: the run finishes on a dispatcher goroutine, and the
// test has no channel into it.
func waitForTerminal(ctx context.Context, t *testing.T, st *sqlitestore.Store, runID id.AgentRunID) *run.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		r, err := st.GetRun(ctx, runID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		switch r.State {
		case run.StateCompleted, run.StateFailed, run.StateCancelled:
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("the run never reached a terminal state; it is %s", r.State)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func headerWith(k, v string) http.Header {
	h := http.Header{}
	h.Set(k, v)
	return h
}

func TestAPeerWithoutCredentialsIsRefused(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "tenant", Value: "acme"}},
	})
	engB, _ := newEngine(ctx, t, "b2", &scriptedLLM{answer: "no"}, "specialist")

	svc, err := a2aremote.Attach(engB, a2aremote.AttachOptions{
		Resolver: a2aremote.ResolverFunc(func(context.Context, a2aremote.Credentials) (a2aremote.Peer, error) {
			return a2aremote.Peer{}, errors.New("no")
		}),
		Service: a2aremote.Options{Exposed: []string{"specialist"}},
	})
	if err != nil {
		t.Fatalf("attach: %v", err)
	}
	srv := httptest.NewServer(svc.Handler())
	defer srv.Close()

	client := a2aremote.NewClient([]a2aremote.PeerConfig{{Node: "peer-b", BaseURL: srv.URL}}, nil, a2aremote.ClientOptions{})
	err = client.Deliver(ctx, &a2a.Envelope{
		ID: id.NewMessageID(), Performative: a2a.Request,
		Sender: a2a.Address{Agent: "planner"}, Content: "let me in",
	}, a2a.Address{Agent: "specialist", Node: "peer-b"})
	if err == nil {
		t.Fatal("an unauthenticated peer must be refused")
	}
}
