// Package scopespy provides a recording store double for asserting that
// every store call made by the engine carries a non-zero scope.
package scopespy

import (
	"context"
	"sync"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/session"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
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

	mu            sync.Mutex
	calls         []Call
	suspensions   []*suspension.Suspension
	suspendErr    error
	claimed       map[id.AgentRunID]bool
	runs          map[id.AgentRunID]run.Run
	steps         []run.Step
	toolCalls     []run.ToolCall
	checkpoints   []*checkpoint.Checkpoint
	checkpointErr error
	runReadErr    error
	runReadsLeft  int
}

// FailCheckpointWrites makes every later CreateCheckpoint return err, so
// a test can drive the engine's "the checkpoint could not be written"
// branch without a real store.
func (s *Spy) FailCheckpointWrites(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checkpointErr = err
}

// Checkpoints returns the checkpoints the engine wrote, in order.
func (s *Spy) Checkpoints() []*checkpoint.Checkpoint {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*checkpoint.Checkpoint, len(s.checkpoints))
	copy(out, s.checkpoints)
	return out
}

// CreateCheckpoint records the checkpoint itself, not just that the call
// happened: an approval suspension's whole point is what the row a human
// will read says.
func (s *Spy) CreateCheckpoint(ctx context.Context, cp *checkpoint.Checkpoint) error {
	s.record(ctx, "CreateCheckpoint")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.checkpointErr != nil {
		return s.checkpointErr
	}
	stored := *cp
	s.checkpoints = append(s.checkpoints, &stored)
	return nil
}

// FailSuspensionWrites makes every later CreateSuspension return err, so
// a test can drive the engine's "the suspension could not be written"
// branch without a real store.
func (s *Spy) FailSuspensionWrites(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.suspendErr = err
}

// Suspensions returns the suspensions the engine wrote, in order.
func (s *Spy) Suspensions() []*suspension.Suspension {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*suspension.Suspension, len(s.suspensions))
	copy(out, s.suspensions)
	return out
}

// ToolCalls returns the tool call rows the engine wrote, in order.
func (s *Spy) ToolCalls() []run.ToolCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]run.ToolCall, len(s.toolCalls))
	copy(out, s.toolCalls)
	return out
}

// Steps returns the steps the engine wrote, in order.
func (s *Spy) Steps() []run.Step {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]run.Step, len(s.steps))
	copy(out, s.steps)
	return out
}

func New() *Spy {
	return &Spy{
		claimed: make(map[id.AgentRunID]bool),
		runs:    make(map[id.AgentRunID]run.Run),
	}
}

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

// CreateRun keeps the run so GetRun can serve it back. A resume reads
// the run it is continuing, and a spy that forgot every run it was handed
// could not exercise that path at all.
func (s *Spy) CreateRun(ctx context.Context, r *run.Run) error {
	s.record(ctx, "CreateRun")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[r.ID] = *r
	return nil
}

func (s *Spy) UpdateRun(ctx context.Context, r *run.Run) error {
	s.record(ctx, "UpdateRun")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runs[r.ID] = *r
	return nil
}

// Runs returns every run the engine wrote, so a test whose RunAgent
// returned an error rather than a run can still ask what state that run
// ended in. Order is undefined: the map behind it has none.
func (s *Spy) Runs() []run.Run {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]run.Run, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, r)
	}
	return out
}

// FailRunReadsAfter lets the next n GetRun calls through and fails every
// one after them, so a test can break the read that follows a claim
// without breaking the reads that set the claim up.
func (s *Spy) FailRunReadsAfter(n int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runReadsLeft = n
	s.runReadErr = err
}

// GetRun returns a copy, the way a real store would. Handing back the
// same object the engine already holds would let a test pass on a run the
// engine never actually persisted.
func (s *Spy) GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	s.record(ctx, "GetRun")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runReadErr != nil {
		if s.runReadsLeft > 0 {
			s.runReadsLeft--
		} else {
			return nil, s.runReadErr
		}
	}
	r, ok := s.runs[runID]
	if !ok {
		return nil, cortex.ErrRunNotFound
	}
	return &r, nil
}

// Get returns an agent carrying the id it was asked for. GetByName mints
// a fresh id per call, so a resume that looked the agent up by the run's
// AgentID would otherwise get a stranger back.
func (s *Spy) Get(ctx context.Context, agentID id.AgentID) (*agent.Config, error) {
	s.record(ctx, "Get")
	return &agent.Config{
		ID:       agentID,
		Name:     "assistant",
		MaxSteps: 2,
	}, nil
}

// CreateSuspension records the suspension itself, not just that the call
// happened: a suspend test's whole subject is what the row says.
//
// It also refuses a second suspension for a run that already has one,
// which is the partial unique index every real backend carries. Without
// it a resume that forgot to delete the row it claimed would still pass
// here, and the collision would only show up in production.
func (s *Spy) CreateSuspension(ctx context.Context, susp *suspension.Suspension) error {
	s.record(ctx, "CreateSuspension")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.suspendErr != nil {
		return s.suspendErr
	}
	if _, err := s.findSuspension(susp.RunID); err == nil {
		return cortex.ErrAlreadyExists
	}
	s.suspensions = append(s.suspensions, susp)
	return nil
}

// GetSuspension serves back what CreateSuspension recorded.
// GetCheckpoint serves back what CreateCheckpoint recorded, including
// the state Resolve moved it to: a decision routed off a stale read is
// a decision applied twice.
func (s *Spy) GetCheckpoint(ctx context.Context, cpID id.CheckpointID) (*checkpoint.Checkpoint, error) {
	s.record(ctx, "GetCheckpoint")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cp := range s.checkpoints {
		if cp.ID == cpID {
			stored := *cp
			return &stored, nil
		}
	}
	return nil, cortex.ErrCheckpointNotFound
}

// ListPending serves the pending rows back, filtered by run the way the
// real stores filter them. A cancelled run resolves whatever it left in
// the queue, and a spy that returned nothing could not tell that apart
// from a cancel that skipped the queue entirely.
func (s *Spy) ListPending(ctx context.Context, filter *checkpoint.ListFilter) ([]*checkpoint.Checkpoint, error) {
	s.record(ctx, "ListPending")
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*checkpoint.Checkpoint, 0, len(s.checkpoints))
	for _, cp := range s.checkpoints {
		if cp.State != "pending" {
			continue
		}
		if filter != nil && filter.RunID != "" && cp.RunID.String() != filter.RunID {
			continue
		}
		stored := *cp
		out = append(out, &stored)
	}
	return out, nil
}

// Resolve records the decision on the stored row, the way a real store
// does. A spy that only counted the call could not tell a checkpoint
// resolved once from one resolved twice.
func (s *Spy) Resolve(ctx context.Context, cpID id.CheckpointID, decision checkpoint.Decision) error {
	s.record(ctx, "Resolve")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cp := range s.checkpoints {
		if cp.ID == cpID {
			cp.State = "resolved"
			d := decision
			cp.Decision = &d
			return nil
		}
	}
	return cortex.ErrCheckpointNotFound
}

func (s *Spy) GetSuspension(ctx context.Context, runID id.AgentRunID) (*suspension.Suspension, error) {
	s.record(ctx, "GetSuspension")
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findSuspension(runID)
}

// ClaimSuspension models the one property the real stores go to a
// transaction for: a suspension can only be claimed once. The second
// claimer gets ErrNotSuspended, exactly as it would against a run the
// first claimer already moved out of paused.
func (s *Spy) ClaimSuspension(ctx context.Context, runID id.AgentRunID) (*suspension.Suspension, error) {
	s.record(ctx, "ClaimSuspension")
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.claimed[runID] {
		return nil, cortex.ErrNotSuspended
	}
	susp, err := s.findSuspension(runID)
	if err != nil {
		return nil, err
	}
	s.claimed[runID] = true
	return susp, nil
}

// DeleteSuspension drops the row, so a resumed run that suspends again
// writes a fresh one rather than colliding with its own stale one.
func (s *Spy) DeleteSuspension(ctx context.Context, runID id.AgentRunID) error {
	s.record(ctx, "DeleteSuspension")
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, susp := range s.suspensions {
		if susp.RunID == runID {
			s.suspensions = append(s.suspensions[:i], s.suspensions[i+1:]...)
			delete(s.claimed, runID)
			return nil
		}
	}
	return cortex.ErrNotSuspended
}

// findSuspension must be called with mu held.
func (s *Spy) findSuspension(runID id.AgentRunID) (*suspension.Suspension, error) {
	for _, susp := range s.suspensions {
		if susp.RunID == runID {
			return susp, nil
		}
	}
	return nil, cortex.ErrNotSuspended
}

func (s *Spy) CreateStep(ctx context.Context, step *run.Step) error {
	s.record(ctx, "CreateStep")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append(s.steps, *step)
	return nil
}

// ListSteps is what a resume uses to find the step its pending calls came
// from, so the tool call rows it writes land on the step that made them.
func (s *Spy) ListSteps(ctx context.Context, runID id.AgentRunID) ([]*run.Step, error) {
	s.record(ctx, "ListSteps")
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]*run.Step, 0, len(s.steps))
	for i := range s.steps {
		if s.steps[i].RunID == runID {
			step := s.steps[i]
			out = append(out, &step)
		}
	}
	return out, nil
}

// CreateToolCall records the row itself: a resumed call's row is the
// audit trail entry a test here is actually asserting on.
func (s *Spy) CreateToolCall(ctx context.Context, tc *run.ToolCall) error {
	s.record(ctx, "CreateToolCall")
	s.mu.Lock()
	defer s.mu.Unlock()
	s.toolCalls = append(s.toolCalls, *tc)
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
