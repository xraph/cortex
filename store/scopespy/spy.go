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
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/trait"
)

// Call is one recorded store invocation.
type Call struct {
	Method string
	Scope  cortex.Scope
	// SessionID is the session id a conversation call carried, when the
	// method takes one. Zero for every method that doesn't.
	SessionID id.SessionID
	// CtxErr is ctx.Err() at call time. A terminal write issued from an
	// already-cancelled ctx would otherwise fail before it reaches a real
	// store, so a non-nil CtxErr on a call that's supposed to persist a
	// cancel/failure outcome is the same bug shape this field exists to
	// catch even though the spy itself doesn't reject cancelled contexts.
	CtxErr error
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
	s.calls = append(s.calls, Call{Method: method, Scope: cortex.ScopeFromContext(ctx), CtxErr: ctx.Err()})
}

// recordSession is record plus the session id a conversation call
// carried, so a test can assert resolveSession actually threaded a real
// session through rather than a zero id.
func (s *Spy) recordSession(ctx context.Context, method string, sessionID id.SessionID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{Method: method, Scope: cortex.ScopeFromContext(ctx), SessionID: sessionID, CtxErr: ctx.Err()})
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

func (s *Spy) LoadConversation(ctx context.Context, _ id.AgentID, sessionID id.SessionID, _ int) ([]memory.Message, error) {
	s.recordSession(ctx, "LoadConversation", sessionID)
	return nil, nil
}

func (s *Spy) SaveConversation(ctx context.Context, _ id.AgentID, sessionID id.SessionID, _ []memory.Message) error {
	s.recordSession(ctx, "SaveConversation", sessionID)
	return nil
}

// ListSessions returns no existing sessions, so resolveSession's
// default-session lookup falls through to CreateSession below — the same
// lazy-create path a fresh agent takes in a real store.
func (s *Spy) ListSessions(ctx context.Context, _ *session.ListFilter) ([]*session.Session, error) {
	s.record(ctx, "ListSessions")
	return nil, nil
}

// CreateSession records the call and succeeds. resolveSession already
// generated the session's id before calling this, so there is nothing
// for the spy to mutate on the way back.
func (s *Spy) CreateSession(ctx context.Context, _ *session.Session) error {
	s.record(ctx, "CreateSession")
	return nil
}

// GetByName returns a usable agent so RunAgent proceeds into the react
// loop. Returning an error here would end the run before any of the
// conversation calls we are actually trying to observe. PersonaRef,
// InlineSkills and InlineTraits are populated so BuildSystemPrompt
// reaches GetPersonaByName/GetSkillByName/GetTraitByName too — those
// calls carry scope the same as everything else the loop touches.
// MaxSteps is 2 so a tool-calling LLM double gets a second step to
// answer in after the tool call, without changing behavior for a
// non-tool-calling double, which breaks out of the loop after step one
// regardless of MaxSteps.
func (s *Spy) GetByName(ctx context.Context, name string) (*agent.Config, error) {
	s.record(ctx, "GetByName")
	return &agent.Config{
		ID:           id.NewAgentID(),
		Name:         name,
		MaxSteps:     2,
		PersonaRef:   "spy-persona",
		InlineSkills: []string{"spy-skill"},
		InlineTraits: []string{"spy-trait"},
	}, nil
}

// GetPersonaByName returns a usable persona so BuildSystemPrompt's
// identity injection has something to record scope on.
func (s *Spy) GetPersonaByName(ctx context.Context, name string) (*persona.Persona, error) {
	s.record(ctx, "GetPersonaByName")
	return &persona.Persona{ID: id.NewPersonaID(), Name: name, Identity: "spy identity"}, nil
}

// GetSkillByName returns a usable skill so BuildSystemPrompt's skill
// injection has something to record scope on.
func (s *Spy) GetSkillByName(ctx context.Context, name string) (*skill.Skill, error) {
	s.record(ctx, "GetSkillByName")
	return &skill.Skill{ID: id.NewSkillID(), Name: name, SystemPromptFragment: "spy skill fragment"}, nil
}

// GetTraitByName returns a usable trait so BuildSystemPrompt's trait
// injection has something to record scope on.
func (s *Spy) GetTraitByName(ctx context.Context, name string) (*trait.Trait, error) {
	s.record(ctx, "GetTraitByName")
	return &trait.Trait{ID: id.NewTraitID(), Name: name}, nil
}
