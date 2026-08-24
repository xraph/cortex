package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	log "github.com/xraph/go-utils/log"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/trait"
)

// registeredTool pairs an externally-registered tool definition with its handler.
type registeredTool struct {
	def     llm.Tool
	handler ToolHandler
}

// Engine is the central coordinator for the Cortex agent system.
type Engine struct {
	config      cortex.Config
	logger      log.Logger
	store       store.Store
	llm         llm.Client
	safety      safety.Scanner
	knowledge   knowledge.Provider
	authorizer  cortex.ToolAuthorizer
	extensions  *plugin.Registry
	pendingExts []plugin.Extension
	tools       []registeredTool
	// externalTools are advertised but never dispatched here: a call to
	// one suspends the run so the host can execute it.
	externalTools []llm.Tool

	// suspensionTTL is how long a paused run waits before the sweeper
	// fails it. Zero switches expiry off entirely: suspensions are
	// written with no deadline and no sweeper runs.
	suspensionTTL time.Duration
	sweepInterval time.Duration
	sweepLimit    int
	sweep         sweeper
}

// LLM returns the configured LLM client, or nil if none is set.
func (e *Engine) LLM() llm.Client { return e.llm }

// Safety returns the configured safety scanner, or nil if none is set.
func (e *Engine) Safety() safety.Scanner { return e.safety }

// Knowledge returns the configured knowledge provider, or nil if none is set.
func (e *Engine) Knowledge() knowledge.Provider { return e.knowledge }

// RunOverrides allows overriding agent configuration for a single run.
type RunOverrides struct {
	Model           string
	Temperature     *float64
	MaxSteps        int
	MaxTokens       int
	ReasoningLoop   string
	SystemPrompt    string
	PersonaRef      string
	InlineSkills    []string
	InlineTraits    []string
	InlineBehaviors []string
	Tools           []string
	SessionID       id.SessionID
}

// New creates a new Engine with the given options.
func New(opts ...Option) (*Engine, error) {
	e := &Engine{
		config: cortex.DefaultConfig(),
		logger: log.NewNoopLogger(),
		// Set before the options run, so WithSuspensionTTL(0) reads as
		// the caller switching expiry off rather than as a field nobody
		// filled in.
		suspensionTTL: defaultSuspensionTTL,
		sweepInterval: defaultSweepInterval,
		sweepLimit:    defaultSweepLimit,
	}

	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, fmt.Errorf("cortex: apply engine option: %w", err)
		}
	}

	e.extensions = plugin.NewRegistry(e.logger)
	for _, ext := range e.pendingExts {
		e.extensions.Register(ext)
	}
	e.pendingExts = nil

	return e, nil
}

// Health checks the health of the engine by pinging its store.
func (e *Engine) Health(ctx context.Context) error {
	if e.store != nil {
		return e.store.Ping(ctx)
	}
	return nil
}

// Start initializes the engine for operation.
//
// The context is deliberately still ignored. Start takes one to match
// every other lifecycle hook in the ecosystem, but a caller's start
// context is not the engine's lifetime: the expiry sweeper it launches
// here runs on a handle of its own and is stopped by Stop.
func (e *Engine) Start(_ context.Context) error {
	e.startSweeper()
	e.logger.Info("cortex engine started")
	return nil
}

// Stop gracefully shuts down the engine.
//
// The sweeper is joined, not merely signalled, and it is joined before
// the shutdown hook fires: a subscriber told the engine has stopped
// while a sweep is still failing runs has been told something untrue.
func (e *Engine) Stop(ctx context.Context) error {
	e.stopSweeper()
	e.extensions.EmitShutdown(ctx)
	e.logger.Info("cortex engine stopped")
	return nil
}

// Store returns the composite store.
func (e *Engine) Store() store.Store { return e.store }

// Extensions returns the plugin registry.
func (e *Engine) Extensions() *plugin.Registry { return e.extensions }

// Config returns the engine configuration.
func (e *Engine) Config() cortex.Config { return e.config }

// UpdateConfig applies partial runtime configuration updates.
// Only non-zero fields in the update are applied. Changes are in-memory only.
func (e *Engine) UpdateConfig(update cortex.Config) cortex.Config {
	if update.DefaultModel != "" {
		e.config.DefaultModel = update.DefaultModel
	}
	if update.DefaultMaxSteps > 0 {
		e.config.DefaultMaxSteps = update.DefaultMaxSteps
	}
	if update.DefaultMaxTokens > 0 {
		e.config.DefaultMaxTokens = update.DefaultMaxTokens
	}
	if update.DefaultTemperature > 0 {
		e.config.DefaultTemperature = update.DefaultTemperature
	}
	if update.DefaultReasoningLoop != "" {
		e.config.DefaultReasoningLoop = update.DefaultReasoningLoop
	}
	return e.config
}

// ──────────────────────────────────────────────────
// Agent CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreateAgent(ctx context.Context, config *agent.Config) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.Create(ctx, config)
}

func (e *Engine) GetAgent(ctx context.Context, agentID id.AgentID) (*agent.Config, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.Get(ctx, agentID)
}

func (e *Engine) GetAgentByName(ctx context.Context, name string) (*agent.Config, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetByName(ctx, name)
}

func (e *Engine) UpdateAgent(ctx context.Context, config *agent.Config) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.Update(ctx, config)
}

func (e *Engine) DeleteAgent(ctx context.Context, agentID id.AgentID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.Delete(ctx, agentID)
}

func (e *Engine) ListAgents(ctx context.Context, filter *agent.ListFilter) ([]*agent.Config, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.List(ctx, filter)
}

func (e *Engine) CountAgents(ctx context.Context, filter *agent.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountAgents(ctx, filter)
}

// ──────────────────────────────────────────────────
// Skill CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreateSkill(ctx context.Context, s *skill.Skill) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateSkill(ctx, s)
}

func (e *Engine) GetSkill(ctx context.Context, skillID id.SkillID) (*skill.Skill, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetSkill(ctx, skillID)
}

func (e *Engine) GetSkillByName(ctx context.Context, name string) (*skill.Skill, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetSkillByName(ctx, name)
}

func (e *Engine) UpdateSkill(ctx context.Context, s *skill.Skill) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.UpdateSkill(ctx, s)
}

func (e *Engine) DeleteSkill(ctx context.Context, skillID id.SkillID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.DeleteSkill(ctx, skillID)
}

func (e *Engine) ListSkills(ctx context.Context, filter *skill.ListFilter) ([]*skill.Skill, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListSkills(ctx, filter)
}

func (e *Engine) CountSkills(ctx context.Context, filter *skill.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountSkills(ctx, filter)
}

// ──────────────────────────────────────────────────
// Trait CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreateTrait(ctx context.Context, t *trait.Trait) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateTrait(ctx, t)
}

func (e *Engine) GetTrait(ctx context.Context, traitID id.TraitID) (*trait.Trait, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetTrait(ctx, traitID)
}

func (e *Engine) GetTraitByName(ctx context.Context, name string) (*trait.Trait, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetTraitByName(ctx, name)
}

func (e *Engine) UpdateTrait(ctx context.Context, t *trait.Trait) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.UpdateTrait(ctx, t)
}

func (e *Engine) DeleteTrait(ctx context.Context, traitID id.TraitID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.DeleteTrait(ctx, traitID)
}

func (e *Engine) ListTraits(ctx context.Context, filter *trait.ListFilter) ([]*trait.Trait, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListTraits(ctx, filter)
}

func (e *Engine) CountTraits(ctx context.Context, filter *trait.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountTraits(ctx, filter)
}

// ──────────────────────────────────────────────────
// Behavior CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreateBehavior(ctx context.Context, b *behavior.Behavior) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateBehavior(ctx, b)
}

func (e *Engine) GetBehavior(ctx context.Context, behaviorID id.BehaviorID) (*behavior.Behavior, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetBehavior(ctx, behaviorID)
}

func (e *Engine) GetBehaviorByName(ctx context.Context, name string) (*behavior.Behavior, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetBehaviorByName(ctx, name)
}

func (e *Engine) UpdateBehavior(ctx context.Context, b *behavior.Behavior) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.UpdateBehavior(ctx, b)
}

func (e *Engine) DeleteBehavior(ctx context.Context, behaviorID id.BehaviorID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.DeleteBehavior(ctx, behaviorID)
}

func (e *Engine) ListBehaviors(ctx context.Context, filter *behavior.ListFilter) ([]*behavior.Behavior, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListBehaviors(ctx, filter)
}

func (e *Engine) CountBehaviors(ctx context.Context, filter *behavior.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountBehaviors(ctx, filter)
}

// ──────────────────────────────────────────────────
// Persona CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreatePersona(ctx context.Context, p *persona.Persona) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreatePersona(ctx, p)
}

func (e *Engine) GetPersona(ctx context.Context, personaID id.PersonaID) (*persona.Persona, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetPersona(ctx, personaID)
}

func (e *Engine) GetPersonaByName(ctx context.Context, name string) (*persona.Persona, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetPersonaByName(ctx, name)
}

func (e *Engine) UpdatePersona(ctx context.Context, p *persona.Persona) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.UpdatePersona(ctx, p)
}

func (e *Engine) DeletePersona(ctx context.Context, personaID id.PersonaID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.DeletePersona(ctx, personaID)
}

func (e *Engine) ListPersonas(ctx context.Context, filter *persona.ListFilter) ([]*persona.Persona, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListPersonas(ctx, filter)
}

func (e *Engine) CountPersonas(ctx context.Context, filter *persona.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountPersonas(ctx, filter)
}

// ──────────────────────────────────────────────────
// Run CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) GetRun(ctx context.Context, runID id.AgentRunID) (*run.Run, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetRun(ctx, runID)
}

func (e *Engine) ListRuns(ctx context.Context, filter *run.ListFilter) ([]*run.Run, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListRuns(ctx, filter)
}

func (e *Engine) CountRuns(ctx context.Context, filter *run.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountRuns(ctx, filter)
}

func (e *Engine) ListSteps(ctx context.Context, runID id.AgentRunID) ([]*run.Step, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListSteps(ctx, runID)
}

func (e *Engine) ListToolCalls(ctx context.Context, stepID id.StepID) ([]*run.ToolCall, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListToolCalls(ctx, stepID)
}

// ──────────────────────────────────────────────────
// Memory passthrough
// ──────────────────────────────────────────────────

func (e *Engine) LoadConversation(ctx context.Context, agentID id.AgentID, sessionID id.SessionID, limit int) ([]memory.Message, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.LoadConversation(ctx, agentID, sessionID, limit)
}

func (e *Engine) ClearConversation(ctx context.Context, agentID id.AgentID, sessionID id.SessionID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.ClearConversation(ctx, agentID, sessionID)
}

// ──────────────────────────────────────────────────
// Checkpoint passthrough
// ──────────────────────────────────────────────────

func (e *Engine) GetCheckpoint(ctx context.Context, cpID id.CheckpointID) (*checkpoint.Checkpoint, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.GetCheckpoint(ctx, cpID)
}

func (e *Engine) ListPendingCheckpoints(ctx context.Context, filter *checkpoint.ListFilter) ([]*checkpoint.Checkpoint, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	return e.store.ListPending(ctx, filter)
}

func (e *Engine) CountPendingCheckpoints(ctx context.Context, filter *checkpoint.ListFilter) (int64, error) {
	if e.store == nil {
		return 0, cortex.ErrNoStore
	}
	return e.store.CountPending(ctx, filter)
}

// ResolveCheckpoint records a human decision, and, when the checkpoint
// belongs to a run the loop paused, carries that decision to the run: an
// approval continues it, a rejection ends it with the reason the decider
// gave.
//
// A checkpoint a host wrote itself resolves exactly as it always has,
// with nothing else touched. Only the loop knows how to pause a run, so
// only a checkpoint the loop opened has a run waiting behind it, and
// treating every checkpoint as though it did would break every caller
// that has been writing its own rows since before suspension existed.
// openedASuspendedRun is what tells the two apart.
//
// Acting before recording is deliberate. The checkpoint store has no
// un-resolve, so recording the decision up front and then failing to act
// on it leaves a row marked resolved against a run still paused, dropped
// out of ListPending, with nothing but a direct Resume call able to
// recover it. That is the audit lie this surface exists to prevent.
// Written this way round, a decision that could not be carried out
// leaves the checkpoint exactly as it found it and the caller can decide
// again.
//
// An approved run is resumed synchronously, same as RunAgent: the caller
// waits for the run rather than getting a receipt for one.
//
// The cost is that two operators deciding the same checkpoint at once
// both pass the pending guard. Nothing runs twice for it: ClaimSuspension
// is atomic, so the second approval gets ErrNotSuspended back rather than
// a second dispatch. Closing that window properly needs a state the store
// can move a checkpoint into while a decision is in flight, which is a
// store interface change rather than an engine one.
func (e *Engine) ResolveCheckpoint(ctx context.Context, cpID id.CheckpointID, decision checkpoint.Decision) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	if decision.DecidedAt.IsZero() {
		decision.DecidedAt = time.Now().UTC()
	}

	// Read before write: the row is what says which run this decision is
	// about, and Resolve does not hand it back.
	cp, err := e.store.GetCheckpoint(ctx, cpID)
	if err != nil {
		return fmt.Errorf("load checkpoint: %w", err)
	}
	if cp.State != checkpointStatePending {
		// A decided checkpoint is not re-decidable. Without this, a
		// rejection could be followed by an approval that tries to
		// resume a run the rejection already failed, and the caller
		// would read the claim's refusal instead of being told the
		// decision was already made.
		return fmt.Errorf("resolve checkpoint %s: already %s: %w", cpID, cp.State, cortex.ErrInvalidState)
	}

	if openedASuspendedRun(cp) {
		if decision.Approved {
			if err := e.resumeApproved(ctx, cp); err != nil {
				return err
			}
		} else if err := e.failRejected(ctx, cp, decision); err != nil {
			return err
		}
	}

	// Recorded last, so the row says decided only about a decision that
	// actually took effect. A failure here is the one leftover this
	// ordering cannot remove: the run has already moved, and the
	// checkpoint stays pending on a run that is no longer waiting. It is
	// visible and it is recoverable, which is more than the reverse
	// order offers.
	if err := e.store.Resolve(ctx, cpID, decision); err != nil {
		return fmt.Errorf("record checkpoint %s as resolved after acting on it: %w", cpID, err)
	}
	e.extensions.EmitCheckpointResolved(ctx, cpID, decisionLabel(decision))
	return nil
}

// openedASuspendedRun reports whether the loop wrote this checkpoint, and
// therefore whether a paused run is waiting on the decision.
//
// It reads the provenance suspend stamped on the row, not the store's
// current state. Asking whether a suspension exists right now would agree
// with this on most rows and then be quietly wrong on one: an approval
// suspension that expired leaves its checkpoint pending with its
// suspension already dropped, and the operator who decides that row has
// to hear about it. A provenance check gives them the claim's refusal.
// A state check would give them a success that moved nothing.
func openedASuspendedRun(cp *checkpoint.Checkpoint) bool {
	_, ok := cp.Metadata[checkpointSuspensionKey]
	return ok
}

// decisionLabel is what a hook subscriber sees. Approved and rejected
// rather than a bool, because the hook takes a string and "true" tells a
// reader nothing about what was true.
func decisionLabel(d checkpoint.Decision) string {
	if d.Approved {
		return "approved"
	}
	return "rejected"
}

// resumeApproved continues a run whose pending calls a human just
// granted.
//
// Every pending call is handed back as a result asking the engine to
// execute it, which is what an approval actually means: the calls were
// stopped before dispatch, so there is no result to report, only
// permission to go and get one. Building the input here rather than
// making the caller do it is the point of the method: a REST client
// resolving a checkpoint has no idea what the run was waiting on.
func (e *Engine) resumeApproved(ctx context.Context, cp *checkpoint.Checkpoint) error {
	susp, err := e.store.GetSuspension(ctx, cp.RunID)
	if err != nil {
		return fmt.Errorf("load suspension for approved checkpoint: %w", err)
	}

	in := ResumeInput{ToolResults: make([]ToolResult, 0, len(susp.Pending))}
	for _, p := range susp.Pending {
		in.ToolResults = append(in.ToolResults, ToolResult{ToolCallID: p.ID, Execute: true})
	}

	// resume rather than Resume: this is the one caller allowed to say a
	// checkpoint approved these calls, and it says so having just read a
	// pending checkpoint for this run.
	if _, err := e.resume(ctx, cp.RunID, in, true); err != nil {
		return fmt.Errorf("resume approved run %s: %w", cp.RunID, err)
	}
	return nil
}

// failRejected ends a run whose pending calls a human refused, carrying
// the decider's own words onto the run.
//
// It claims the suspension first, through the same atomic paused-to-
// running transition the approving path goes through, and that is what
// makes two deciders exclusive rather than merely unlikely to collide.
// Without it a rejection was a plain read followed by an unconditional
// write: an approval racing it could resume the run, or finish it, and
// the rejection would then stomp the result to failed afterwards. A
// stale checkpoint is an audit problem; that was corruption of a run
// that had already moved.
//
// A lost claim means somebody else decided first, and it is reported
// rather than worked around: ErrNotSuspended travels back to the caller
// through this error, and nothing is written.
//
// Once the claim succeeds the run belongs to this call, exactly as it
// does in Resume, so every exit below either fails the run or reports
// why it could not. The suspension is deleted after the state write and
// never before: the row is the only record of what the run was waiting
// on, and a delete that landed while the state write failed would throw
// that away for nothing.
func (e *Engine) failRejected(ctx context.Context, cp *checkpoint.Checkpoint, decision checkpoint.Decision) error {
	// Read before the claim, for the reason claimedRun spells out: a
	// read that fails here costs nothing, because the run is still
	// paused and the decider can decide again, while the same failure
	// after the claim would leave the run running with nothing able to
	// take it.
	snapshot, err := e.store.GetRun(ctx, cp.RunID)
	if err != nil {
		return fmt.Errorf("load run for rejected checkpoint: %w", err)
	}

	susp, err := e.store.ClaimSuspension(ctx, cp.RunID)
	if err != nil {
		return fmt.Errorf("claim suspension for rejected checkpoint %s: %w", cp.ID, err)
	}

	// Write under the run's own scope, not the decider's. A checkpoint is
	// resolvable by anyone whose scope covers the run, and everything the
	// run records has to land where its earlier writes did.
	ctx = cortex.WithScope(ctx, susp.Scope)

	r := e.claimedRun(ctx, cp.RunID, snapshot)

	reason := decision.Reason
	if reason == "" {
		reason = "no reason given"
	}
	if err := e.persistFailure(ctx, r, cp.AgentID, fmt.Errorf("checkpoint rejected: %s", reason)); err != nil {
		// The suspension stays: it is the only record of what the run was
		// waiting on, and the checkpoint stays pending too, because
		// nothing has recorded this decision yet.
		return fmt.Errorf("fail rejected run %s: %w", cp.RunID, err)
	}

	// The run is failed and no claim will ever take it again, so the row
	// is dead weight. Dropping it is also what makes the rejection final:
	// a resume after this finds nothing to claim.
	if err := e.store.DeleteSuspension(ctx, cp.RunID); err != nil && !errors.Is(err, cortex.ErrNotSuspended) {
		e.logger.Error("delete suspension of a rejected run", log.String("error", err.Error()))
	}
	return nil
}

// ──────────────────────────────────────────────────
// Agent execution
// ──────────────────────────────────────────────────

// StreamEventType identifies the kind of SSE event.
type StreamEventType string

const (
	EventRunStarted  StreamEventType = "run_started"
	EventStep        StreamEventType = "step"
	EventToolCall    StreamEventType = "tool_call"
	EventToken       StreamEventType = "token"
	EventCheckpoint  StreamEventType = "checkpoint"
	EventSafetyBlock StreamEventType = "safety_block"
	EventDone        StreamEventType = "done"
	EventError       StreamEventType = "error"
	// EventSuspended is the terminal event of a run that paused instead
	// of finishing. Its data carries the run id, the reason and the
	// pending calls the caller has to answer before the run can resume.
	EventSuspended StreamEventType = "suspended"
)

// StreamEvent is a single event emitted during streaming execution.
type StreamEvent struct {
	Type StreamEventType `json:"event"`
	Data map[string]any  `json:"data"`
}

// RunAgent executes an agent synchronously.
// When an LLM client is configured, it uses the ReAct reasoning loop.
// Otherwise, it falls back to mock/echo mode.
func (e *Engine) RunAgent(ctx context.Context, agentName, input string, overrides *RunOverrides) (*run.Run, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	if cortex.ScopeFromContext(ctx).IsZero() {
		return nil, cortex.ErrNoScope
	}

	ag, err := e.store.GetByName(ctx, agentName)
	if err != nil {
		return nil, fmt.Errorf("resolve agent: %w", err)
	}

	// Use real execution if LLM client is available.
	if e.llm != nil {
		return e.runReAct(ctx, ag, input, overrides)
	}

	// Fallback: mock/echo execution.
	return e.runMock(ctx, ag, input)
}

// StreamAgent executes an agent and sends streaming events to the channel.
// The channel is closed when execution completes.
// When an LLM client is configured, it uses the ReAct reasoning loop with streaming.
// Otherwise, it falls back to mock/echo mode.
func (e *Engine) StreamAgent(ctx context.Context, agentName, input string, overrides *RunOverrides, events chan<- StreamEvent) error {
	if e.store == nil {
		close(events)
		return cortex.ErrNoStore
	}
	if cortex.ScopeFromContext(ctx).IsZero() {
		close(events)
		return cortex.ErrNoScope
	}

	ag, err := e.store.GetByName(ctx, agentName)
	if err != nil {
		close(events)
		return fmt.Errorf("resolve agent: %w", err)
	}

	// Use real execution if LLM client is available.
	if e.llm != nil {
		return e.streamReAct(ctx, ag, input, overrides, events)
	}

	// Fallback: mock/echo execution.
	return e.streamMock(ctx, ag, input, events)
}

// runMock is the mock/echo fallback for RunAgent.
func (e *Engine) runMock(ctx context.Context, ag *agent.Config, input string) (*run.Run, error) {
	now := time.Now().UTC()
	r := &run.Run{
		Entity:     cortex.NewEntity(),
		ID:         id.NewAgentRunID(),
		AgentID:    ag.ID,
		State:      run.StateRunning,
		Input:      input,
		StartedAt:  &now,
		PersonaRef: ag.PersonaRef,
	}
	if err := e.store.CreateRun(ctx, r); err != nil {
		return nil, fmt.Errorf("create run: %w", err)
	}

	e.extensions.EmitRunStarted(ctx, ag.ID, r.ID, input)

	stepStart := time.Now().UTC()
	step := &run.Step{
		Entity:     cortex.NewEntity(),
		ID:         id.NewStepID(),
		RunID:      r.ID,
		Index:      0,
		Type:       "generation",
		Input:      input,
		Output:     "Echo: " + input,
		TokensUsed: len(input) + 6,
		StartedAt:  &stepStart,
	}
	stepEnd := time.Now().UTC()
	step.CompletedAt = &stepEnd
	if err := e.store.CreateStep(ctx, step); err != nil {
		e.logger.Error("create step", log.String("error", err.Error()))
	}

	completedAt := time.Now().UTC()
	r.State = run.StateCompleted
	r.Output = step.Output
	r.StepCount = 1
	r.TokensUsed = step.TokensUsed
	r.CompletedAt = &completedAt
	if err := e.store.UpdateRun(ctx, r); err != nil {
		e.logger.Error("update run", log.String("error", err.Error()))
	}

	e.extensions.EmitRunCompleted(ctx, ag.ID, r.ID, r.Output, completedAt.Sub(now))

	return r, nil
}

// streamMock is the mock/echo fallback for StreamAgent.
func (e *Engine) streamMock(ctx context.Context, ag *agent.Config, input string, events chan<- StreamEvent) error {
	now := time.Now().UTC()
	r := &run.Run{
		Entity:     cortex.NewEntity(),
		ID:         id.NewAgentRunID(),
		AgentID:    ag.ID,
		State:      run.StateRunning,
		Input:      input,
		StartedAt:  &now,
		PersonaRef: ag.PersonaRef,
	}
	if err := e.store.CreateRun(ctx, r); err != nil {
		close(events)
		return fmt.Errorf("create run: %w", err)
	}

	e.extensions.EmitRunStarted(ctx, ag.ID, r.ID, input)

	go func() {
		defer close(events)

		events <- StreamEvent{Type: EventRunStarted, Data: map[string]any{
			"run_id":   r.ID.String(),
			"agent_id": ag.ID.String(),
		}}

		stepStart := time.Now().UTC()
		step := &run.Step{
			Entity:    cortex.NewEntity(),
			ID:        id.NewStepID(),
			RunID:     r.ID,
			Index:     0,
			Type:      "generation",
			Input:     input,
			StartedAt: &stepStart,
		}

		events <- StreamEvent{Type: EventStep, Data: map[string]any{
			"step_id": step.ID.String(),
			"index":   0,
			"type":    "generation",
		}}

		response := "Echo: " + input
		for i, ch := range response {
			select {
			case <-ctx.Done():
				r.State = run.StateCancelled
				completedAt := time.Now().UTC()
				r.CompletedAt = &completedAt
				// ctx is already cancelled here, so a store write using it
				// outright would fail before it starts and the cancel state
				// would never persist, leaving the run stuck at "running".
				// WithoutCancel keeps every context value (including scope)
				// while dropping the cancellation signal for this one
				// terminal write.
				if err := e.store.UpdateRun(context.WithoutCancel(ctx), r); err != nil {
					e.logger.Error("update run on cancel", log.String("error", err.Error()))
				}
				events <- StreamEvent{Type: EventError, Data: map[string]any{"message": "cancelled"}}
				return
			default:
			}

			events <- StreamEvent{Type: EventToken, Data: map[string]any{
				"content": string(ch),
				"index":   i,
			}}

			time.Sleep(20 * time.Millisecond)
		}

		stepEnd := time.Now().UTC()
		step.Output = response
		step.TokensUsed = len(input) + 6
		step.CompletedAt = &stepEnd
		if err := e.store.CreateStep(ctx, step); err != nil {
			e.logger.Error("create step", log.String("error", err.Error()))
		}

		completedAt := time.Now().UTC()
		r.State = run.StateCompleted
		r.Output = response
		r.StepCount = 1
		r.TokensUsed = step.TokensUsed
		r.CompletedAt = &completedAt
		if err := e.store.UpdateRun(ctx, r); err != nil {
			e.logger.Error("update run", log.String("error", err.Error()))
		}

		e.extensions.EmitRunCompleted(ctx, ag.ID, r.ID, r.Output, completedAt.Sub(now))

		events <- StreamEvent{Type: EventDone, Data: map[string]any{
			"run_id":      r.ID.String(),
			"output":      r.Output,
			"tokens_used": r.TokensUsed,
			"duration_ms": completedAt.Sub(now).Milliseconds(),
		}}
	}()

	return nil
}

// CreateRun creates a run record directly (for external use).
func (e *Engine) CreateRun(ctx context.Context, r *run.Run) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.CreateRun(ctx, r)
}

// UpdateRun updates a run record (for external use like cancellation).
func (e *Engine) UpdateRun(ctx context.Context, r *run.Run) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.store.UpdateRun(ctx, r)
}
