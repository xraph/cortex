// Package scopespy provides a recording store double for asserting that
// every store call made by the engine carries a non-zero scope.
package scopespy

import (
	"context"
	"sync"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

// Call is one recorded store invocation.
type Call struct {
	Method string
	Scope  cortex.Scope
}

// Spy implements store.Store by embedding it as a nil interface and
// overriding only what the react loop calls. Anything else panics, which
// is deliberate: an unrecorded call is a hole in the regression guard.
type Spy struct {
	store.Store

	mu    sync.Mutex
	calls []Call
}

func New() *Spy { return &Spy{} }

func (s *Spy) record(ctx context.Context, method string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{Method: method, Scope: cortex.ScopeFromContext(ctx)})
}

func (s *Spy) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Call, len(s.calls))
	copy(out, s.calls)
	return out
}

func (s *Spy) CallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *Spy) CreateRun(ctx context.Context, _ *run.Run) error {
	s.record(ctx, "CreateRun")
	return nil
}

func (s *Spy) UpdateRun(ctx context.Context, _ *run.Run) error {
	s.record(ctx, "UpdateRun")
	return nil
}

func (s *Spy) CreateStep(ctx context.Context, _ *run.Step) error {
	s.record(ctx, "CreateStep")
	return nil
}

func (s *Spy) CreateToolCall(ctx context.Context, _ *run.ToolCall) error {
	s.record(ctx, "CreateToolCall")
	return nil
}

func (s *Spy) LoadConversation(ctx context.Context, _ id.AgentID, _ int) ([]memory.Message, error) {
	s.record(ctx, "LoadConversation")
	return nil, nil
}

func (s *Spy) SaveConversation(ctx context.Context, _ id.AgentID, _ []memory.Message) error {
	s.record(ctx, "SaveConversation")
	return nil
}

// GetByName returns a usable agent so RunAgent proceeds into the react
// loop. Returning an error here would end the run before any of the
// conversation calls we are actually trying to observe.
func (s *Spy) GetByName(ctx context.Context, _, name string) (*agent.Config, error) {
	s.record(ctx, "GetByName")
	return &agent.Config{ID: id.NewAgentID(), Name: name, MaxSteps: 1}, nil
}
