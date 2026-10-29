package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	log "github.com/xraph/go-utils/log"
	gometrics "github.com/xraph/go-utils/metrics"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/a2a"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/behavior"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/coordination"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/internal/telemetry"
	"github.com/xraph/cortex/knowledge"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/memory"
	"github.com/xraph/cortex/persona"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/skill"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
	"github.com/xraph/cortex/trait"
	"github.com/xraph/cortex/verification"
)

// registeredTool pairs an externally-registered tool definition with its handler.
type registeredTool struct {
	def     llm.Tool
	handler ToolHandler
}

// Engine is the central coordinator for the Cortex agent system.
type Engine struct {
	reflection            *ReflectionConfig
	operationalAwareness  *OperationalAwarenessConfig
	contextBudget         *ContextBudgetConfig
	preparedMessaging     *PreparedTeamMessagingConfig
	orchestrationExpiry   *coordination.OrchestrationExpiryService
	orchestrationRecovery *coordination.OrchestrationRecoveryService
	agentReplyRecovery    *coordination.AgentReplyRecoveryService
	lifecycle             lifecycleGate
	admission             admission
	runConcurrency        int
	taskScheduling        *coordination.TaskService
	claimRecovery         *coordination.ClaimRecoveryService
	providerRecovery      *coordination.ProviderRecoveryService
	verificationRecovery  *coordination.VerificationRecoveryService
	effectContinuation    *coordination.EffectContinuationService
	effectRecovery        *coordination.EffectRecoveryService
	taskAdoption          *coordination.TaskAdoptionService
	taskSettlement        *coordination.TaskSettlementService
	taskRetirement        *coordination.TaskRetirementService
	taskCompletion        *coordination.TaskCompletionService
	planReplanning        *coordination.PlanReplanService
	coordinationEvents    *coordination.EventService
	artifactPublication   *coordination.ArtifactPublicationService
	taskSchedulingWork    TaskWorkerConfig
	taskRunAdmitter       coordination.TaskRunAdmitter
	taskCallGuard         TaskCallGuard
	config                cortex.Config
	logger                log.Logger
	metrics               gometrics.Metrics
	instr                 *engineMetrics
	logThrottle           telemetry.Throttle
	store                 store.Store
	atomicPreparation     bool
	publicEventRecording  bool
	publicEventRecorder   event.TerminalStore
	publicEventConfigured bool
	publicEventAuthorizer event.Authorizer
	publicEventReader     *event.Reader
	resultContentConfig   *ResultContentConfig
	resultContentReader   *verification.ResultReader
	verificationConfig    *VerificationWorkerConfig
	verificationWorker    *verificationWorker
	requiredVerification  *RequiredVerificationConfig
	llm                   llm.Client
	safety                safety.Scanner
	publicationPolicy     safety.Policy
	publicationGate       safety.PublicationGate
	knowledge             knowledge.Provider
	authorizer            cortex.ToolAuthorizer
	principalCodec        cortex.PrincipalCodec
	// continuation is the store's run.ContinuationStore, nil when it has
	// none. DecideCheckpoint, ProvideResults and ContinueRun need it.
	continuation run.ContinuationStore
	// rejectionsToModel is WithRejectionsReturnToModel.
	rejectionsToModel bool
	// progress is the store's suspension.ProgressStore, nil when it has
	// none. Leasing needs it and continuation both.
	progress suspension.ProgressStore
	// leasedClaims is the store's suspension.LeasedClaimStore, nil when it
	// has none: claims that take the lease with the pause. checkpointSweep
	// is its checkpoint.SweepStore, nil when it has none.
	leasedClaims    suspension.LeasedClaimStore
	checkpointSweep checkpoint.SweepStore
	// leaseTTL is WithRunLease, workerID WithWorkerID. leases tracks the
	// leased runs this engine is executing, so a Stop that runs out of
	// time can hand them off.
	leaseTTL time.Duration
	workerID string
	leases   leaseRegistry
	// reclaimLimit is WithReclaimLimit, readyRenotify WithReadyRenotify.
	reclaimLimit  int
	readyRenotify time.Duration
	// renewalsPaused stops every heartbeat without releasing its lease. It
	// is a test seam: a stalled worker, in a test, is one whose renewals
	// stopped while it kept running.
	renewalsPaused atomic.Bool
	extensions     *plugin.Registry
	pendingExts    []plugin.Extension
	tools          []registeredTool
	toolRevisions  map[string]string
	modelRevisions map[string]string
	// externalTools are advertised but never dispatched here: a call to
	// one suspends the run so the host can execute it.
	externalTools []registeredExternal

	// retrySafeNames is what WithToolRetrySafe collected; New checks each
	// against tools and folds them into retrySafe.
	retrySafeNames []string
	retrySafe      map[string]bool

	// suspensionTTL is the default for how long a paused run waits
	// before the sweeper fails it, used by any pending call with no TTL
	// of its own. Zero means no default deadline; the sweeper still runs
	// for calls that carry one.
	suspensionTTL time.Duration
	// agentReplyGrace is how long past an ask's reply-by a run waiting
	// on that answer stays paused, so the ask sweep reaches it first.
	agentReplyGrace time.Duration
	sweepInterval   time.Duration
	sweepLimit      int
	sweep           sweeper

	// a2a is the agent-to-agent message bus, nil unless a host asked for
	// messaging with WithA2A. Its nil-ness is what gates the three
	// messaging tools out of the list a model ever sees.
	a2a               *a2a.Bus
	a2aCfg            *a2aConfig
	a2aHandoffHandler a2a.HandoffHandler
	a2aSweepCancel    context.CancelFunc
	a2aSweepDone      chan struct{}
}

// LLM returns the configured LLM client, or nil if none is set.
func (e *Engine) LLM() llm.Client { return e.llm }

// Safety returns the configured safety scanner, or nil if none is set.
func (e *Engine) Safety() safety.Scanner { return e.safety }

// Knowledge returns the configured knowledge provider, or nil if none is set.
func (e *Engine) Knowledge() knowledge.Provider { return e.knowledge }

// RunConcurrency returns how many runs may execute at once, or zero when
// there is no limit. See WithRunConcurrency.
func (e *Engine) RunConcurrency() int { return e.admission.limit }

// admit lets a run start under the engine's concurrency limit, or returns
// why it cannot: cortex.ErrEngineStopping while Stop drains, or ctx's
// error when no slot freed in time. The returned ctx carries the run's
// ticket and must be the one the run executes on.
func (e *Engine) admit(ctx context.Context) (context.Context, func(), error) {
	return e.admission.admit(ctx)
}

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
	ToolsRestricted bool
	Budget          *execution.Budget
	SessionID       id.SessionID
	// RunID pre-assigns the new run's id, which must be an "arun" id, so
	// a host that retries a start cannot run the same work twice: a run
	// with this id that already exists returns cortex.ErrAlreadyExists and
	// nothing runs. Zero lets the engine pick one.
	RunID id.AgentRunID

	// replyRoute is where the answer goes when the messaging bus started
	// the run for a directive.
	replyRoute *a2a.ReplyRoute
}

// New creates a new Engine with the given options.
func New(opts ...Option) (*Engine, error) {
	e := &Engine{
		config: cortex.DefaultConfig(),
		logger: log.NewNoopLogger(),
		// Set before the options run, so WithSuspensionTTL(0) reads as
		// the caller dropping the default deadline rather than as a field
		// nobody filled in.
		suspensionTTL:   defaultSuspensionTTL,
		agentReplyGrace: defaultAgentReplyGrace,
		sweepInterval:   defaultSweepInterval,
		sweepLimit:      defaultSweepLimit,
		leaseTTL:        defaultRunLease,
		reclaimLimit:    defaultReclaimLimit,
		readyRenotify:   defaultReadyRenotify,
	}

	for _, opt := range opts {
		if err := opt(e); err != nil {
			return nil, fmt.Errorf("cortex: apply engine option: %w", err)
		}
	}

	// Asserted once, after every option, so WithStore can sit anywhere in
	// the caller's list.
	if continuation, ok := e.store.(run.ContinuationStore); ok {
		e.continuation = continuation
	}
	if progress, ok := e.store.(suspension.ProgressStore); ok {
		e.progress = progress
	}
	if leased, ok := e.store.(suspension.LeasedClaimStore); ok {
		e.leasedClaims = leased
	}
	if sweep, ok := e.store.(checkpoint.SweepStore); ok {
		e.checkpointSweep = sweep
	}
	if e.workerID == "" {
		e.workerID = defaultWorkerID()
	}

	// After every option, so WithToolAuthorizer and WithPrincipalCodec can
	// come in either order.
	if e.authorizer != nil && e.principalCodec == nil {
		return nil, cortex.ErrPrincipalCodecRequired
	}

	if err := e.configureRetrySafe(); err != nil {
		return nil, err
	}

	if err := e.configurePublication(); err != nil {
		return nil, err
	}

	if err := e.configurePublicEventReplay(); err != nil {
		return nil, err
	}
	if err := e.configureResultContent(); err != nil {
		return nil, err
	}

	if err := e.configureVerificationWorker(); err != nil {
		return nil, err
	}
	if err := e.configureVerificationRecovery(); err != nil {
		return nil, err
	}

	if err := e.configureRequiredVerification(); err != nil {
		return nil, err
	}
	if err := e.configureContextSummary(); err != nil {
		return nil, err
	}

	// Sized after the options, so the last WithRunConcurrency wins.
	e.admission.configure(e.runConcurrency)

	// Instruments are declared once here, after every option has run, so
	// WithMetrics can sit anywhere in the caller's list.
	e.instr = newEngineMetrics(e.metrics)

	e.extensions = plugin.NewRegistry(e.logger)
	for _, ext := range e.pendingExts {
		e.extensions.Register(ext)
	}
	e.pendingExts = nil

	// The bus is built after every option has run, not inside WithA2A:
	// it needs the store and the registry, and an option cannot know
	// whether WithStore comes before or after it in the caller's list.
	if err := e.buildA2A(); err != nil {
		return nil, err
	}

	if err := e.configurePreparedTeamMessaging(); err != nil {
		return nil, err
	}
	if err := e.configureTaskScheduling(); err != nil {
		return nil, err
	}
	if err := e.configureAgentReplyRecovery(); err != nil {
		return nil, err
	}
	if err := e.configureOrchestrationRecovery(); err != nil {
		return nil, err
	}

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
// Background services own their lifetimes and are stopped by Stop. Optional
// task scheduling and recovery preserve context values for host identity, without using
// the startup request cancellation as the service lifetime.
func (e *Engine) Start(ctx context.Context) error {
	release, lockErr := e.lifecycle.acquire(ctx)
	if lockErr != nil {
		return lockErr
	}
	defer release()
	// A Stop refuses new runs until the next Start, including one that
	// gave up at its deadline.
	e.admission.reopen()
	if err := e.configureVerificationRecovery(); err != nil {
		return err
	}
	if err := e.configurePreparedTeamMessaging(); err != nil {
		return err
	}
	if err := e.configureTaskScheduling(); err != nil {
		return err
	}
	if err := e.configureAgentReplyRecovery(); err != nil {
		return err
	}
	if err := e.configureOrchestrationRecovery(); err != nil {
		return err
	}
	services := e.taskServices()
	allRunning := len(services) > 0
	for _, service := range services {
		status := service.Status()
		if status.Stopping {
			return coordination.ErrSchedulerStopped
		}
		allRunning = allRunning && status.Running
	}
	if allRunning {
		return nil
	}
	if len(services) > 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	e.startSweeper()
	e.startA2A(ctx)
	// Admission to startup is checked once above. Each service owns its lifetime,
	// so cancellation between starts must not leave a partially started engine.
	startup := context.WithoutCancel(ctx)
	for _, service := range services {
		if err := service.Start(startup); err != nil {
			stopErr := e.stopTaskServices(ctx)
			if stopErr == nil {
				e.stopSweeper()
				e.stopA2A()
			}
			return errors.Join(err, stopErr)
		}
	}

	e.logger.Info("cortex engine started")
	return nil
}

// Stop gracefully shuts down the engine.
//
// It first drains: from the moment Stop starts, RunAgent, StreamAgent,
// RunPrepared, StreamPrepared, Resume, ResumeStream and ResolveCheckpoint
// return cortex.ErrEngineStopping, and Stop waits for the runs already
// executing to finish. A run started from inside one of those (a tool
// calling RunAgent on the ctx it was given) is still let through, because
// its parent is one of the runs being waited for. Start admits runs again.
//
// A leased run (WithRunLease) does not run to the end while Stop drains.
// At its next boundary, before a model call or a tool dispatch, it saves
// its progress, becomes reclaimable under this worker's lease, fires
// RunReclaimable with HandOff set, and returns cortex.ErrRunInterrupted;
// another replica continues it. A run that finishes before reaching a
// boundary simply finishes.
//
// The message bus is under the same admission: while Stop drains it claims
// no new delivery or ask, leaving them queued for another worker, and a
// delivery it was already carrying finishes, resume included.
//
// A run started from inside another run's call (a tool calling RunAgent
// on its ctx) is not handed off because of the drain: it ends with the
// call that started it.
//
// If ctx ends before the last run finishes, Stop cancels the leased runs
// still executing with cause cortex.ErrEngineStopping, still signals task
// services and returns ctx's error, but leaves the sweeper and the message
// bus running and announces no shutdown. Each cancelled run hands itself
// off as above, on its own goroutine: Stop does not wait for those writes,
// so a process that exits at once may leave some of those runs to lapse
// and be reclaimed by the sweep instead. A tool result that arrives after
// the cancellation is discarded, so a call that is not retry-safe stays
// recorded as started.
// Runs without a lease keep executing and write their own outcome when
// they finish. Call Stop again to complete the shutdown before disposing
// stores.
//
// The sweeper is joined, not merely signalled, and it is joined before
// the shutdown hook fires: a subscriber told the engine has stopped
// while a sweep is still failing runs has been told something untrue. Optional
// task scheduling and recovery receive cancellation before any service is drained.
// If a stop deadline expires, Stop returns an error without emitting shutdown;
// call Stop again before disposing stores. Lifecycle calls are serialized through
// shutdown notification, with cancellation-aware waits for another caller.
func (e *Engine) Stop(ctx context.Context) error {
	release, lockErr := e.lifecycle.acquire(ctx)
	if lockErr != nil {
		return lockErr
	}
	defer release()
	if err := e.admission.drain(ctx); err != nil {
		// Out of time: every leased run still executing is told to hand
		// off now rather than at its next boundary, so a host that exits
		// after this leaves those runs reclaimable instead of running.
		e.leases.interruptAll(cortex.ErrEngineStopping)
		return errors.Join(err, e.stopTaskServices(ctx))
	}
	if err := e.stopTaskServices(ctx); err != nil {
		return err
	}
	e.stopSweeper()
	e.stopA2A()
	e.extensions.EmitShutdown(ctx)
	e.logger.Info("cortex engine stopped")
	return nil
}

// Store returns the composite store.
func (e *Engine) Store() store.Store { return e.store }

// Extensions returns the plugin registry.
func (e *Engine) Extensions() *plugin.Registry { return e.extensions }

// Config returns the engine configuration. RunConcurrency is the cap the
// engine applies (WithRunConcurrency), zero for none.
func (e *Engine) Config() cortex.Config {
	c := e.config
	// The cap in force, which WithRunConcurrency sets, not whatever a
	// WithConfig passed in the struct.
	c.RunConcurrency = e.admission.limit
	return c
}

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
	return e.Config()
}

// ──────────────────────────────────────────────────
// Agent CRUD passthrough
// ──────────────────────────────────────────────────

func (e *Engine) CreateAgent(ctx context.Context, config *agent.Config) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	// Keep the stored SystemPrompt in step with the sections it is
	// derived from. Every reader that predates sections still serves
	// that column, so a section edit that skipped this would leave them
	// all handing out the previous prompt with nothing to explain it.
	config.SyncSystemPrompt()
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
	config.SyncSystemPrompt()
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
// gave (or, with WithRejectionsReturnToModel, continues it with the
// rejection as each call's result).
//
// It is DecideCheckpoint followed by ContinueRun on the caller's
// goroutine, so the caller waits for the run rather than getting a
// receipt for one, same as RunAgent. The decision is recorded before the
// run continues, because the ready state between the two is durable: a
// continuation that fails leaves the decision recorded and the run either
// failed (a failure after the claim) or still ready for ContinueRun (a
// refusal before it). Repeating the decision then is cortex.ErrInvalidState
// and never runs the approved action again.
//
// A checkpoint a host wrote itself resolves exactly as it always has,
// with nothing else touched. Only the loop knows how to pause a run, so
// only a checkpoint the loop opened has a run waiting behind it, and
// treating every checkpoint as though it did would break every caller
// that has been writing its own rows since before suspension existed.
// openedASuspendedRun is what tells the two apart.
//
// Two operators deciding the same checkpoint at once both pass the
// pending guard. MarkReady lets one of them move the run and the store
// keeps the first record of a decision, so the other is told
// cortex.ErrInvalidState, fires no hook and continues nothing. Deciding a
// checkpoint whose pause the run has already left is cortex.ErrInvalidState
// too, so it never approves the run's next pause; when the run is waiting
// on a different pause, the stale checkpoint is closed as superseded.
//
// RunReady fires between the two halves. A host that continues ready runs
// from that hook should decide with DecideCheckpoint: with
// ResolveCheckpoint one of the two continuations is refused with
// cortex.ErrRunNotContinuable.
//
// A store without run.ContinuationStore keeps the older order, acting on
// the decision before recording it; see resolveInPlace.
func (e *Engine) ResolveCheckpoint(ctx context.Context, cpID id.CheckpointID, decision checkpoint.Decision) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	if e.continuation == nil {
		return e.resolveInPlace(ctx, cpID, decision)
	}
	ready, err := e.decideCheckpoint(ctx, cpID, decision)
	if err != nil || ready == nil {
		return err
	}
	if _, err := e.ContinueRun(ctx, ready.ID); err != nil {
		return fmt.Errorf("continue run %s after checkpoint %s: %w", ready.ID, cpID, err)
	}
	return nil
}

// resolveInPlace is ResolveCheckpoint for a store that cannot hold a ready
// run: the run is resumed on the caller's goroutine first, and the
// decision is recorded once it has taken effect.
//
// Acting before recording is deliberate here. The checkpoint store has no
// un-resolve, so recording the decision up front and then failing to act
// on it would leave a row marked resolved against a run still paused,
// dropped out of ListPending, with nothing but a direct Resume call able
// to recover it. Written this way round, a decision that could not be
// carried out leaves the checkpoint exactly as it found it and the caller
// can decide again.
//
// The cost is that two operators deciding the same checkpoint at once
// both pass the pending guard. Nothing runs twice for it: ClaimSuspension
// is atomic, so the second approval gets ErrNotSuspended back rather than
// a second dispatch.
func (e *Engine) resolveInPlace(ctx context.Context, cpID id.CheckpointID, decision checkpoint.Decision) error {
	cp, decision, err := e.openDecision(ctx, cpID, decision)
	if err != nil {
		return err
	}

	var executionErr error
	if openedASuspendedRun(cp) {
		susp, bindErr := e.boundSuspension(ctx, cp, false)
		if bindErr != nil {
			return bindErr
		}
		if decision.Approved || e.rejectionsToModel {
			if err := e.resumeDecided(ctx, cp, susp, decision); err != nil {
				uncertain := errors.Is(err, cortex.ErrEffectUncertain)
				exhausted := errors.Is(err, cortex.ErrMaxStepsReached)
				if !uncertain && !exhausted {
					return err
				}
				// The approved call already ran. Confirm its durable stop
				// before recording the decision, including a final-step call
				// with no remaining model step to produce an answer.
				saved, readErr := e.store.GetRun(ctx, cp.RunID)
				if readErr != nil || saved == nil || !isTerminalRunState(saved.State) {
					return err
				}
				if uncertain && saved.Error != cortex.ErrEffectUncertain.Error() {
					return err
				}
				if exhausted && (saved.State != run.StateIncomplete || saved.StopReason != run.StopStepLimit) {
					return err
				}
				executionErr = err
			}
		} else if err := e.failRejected(ctx, cp, susp, decision); err != nil {
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
		return errors.Join(executionErr, fmt.Errorf("record checkpoint %s as resolved after acting on it: %w", cpID, err))
	}
	e.extensions.EmitCheckpointResolved(ctx, cpID, decisionLabel(decision))
	return executionErr
}

// openedASuspendedRun reports whether the loop wrote this checkpoint, and
// therefore whether a paused run is waiting on the decision.
//
// It reads the provenance suspend stamped on the row, not the store's
// current state. Asking whether a suspension exists right now would agree
// with this on most rows and then be quietly wrong on one: a checkpoint
// can outlive its suspension, when the write recording a decision failed
// or when an expiry could not close the row it left, and the operator who
// decides such a row has to hear about it. A provenance check gives them
// the claim's refusal. A state check would give them a success that moved
// nothing.
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

// resumeDecided continues a run whose pending calls a human just
// decided, for resolveInPlace.
//
// Every pending call is handed back with what the decision means for it:
// an approval asks the engine to execute it, since the calls were stopped
// before dispatch and there is no result to report, only permission to go
// and get one. A rejection that returns to the model answers it with an
// error. Building the input here rather than making the caller do it is
// the point of the method: a REST client resolving a checkpoint has no
// idea what the run was waiting on.
func (e *Engine) resumeDecided(ctx context.Context, cp *checkpoint.Checkpoint, susp *suspension.Suspension, decision checkpoint.Decision) error {
	// resume rather than Resume: this is the one caller allowed to say a
	// checkpoint decided these calls, and it says so having just read a
	// pending checkpoint for this run.
	if _, err := e.resume(ctx, cp.RunID, decisionInput(susp.Pending, decision), resumeSourceApproval); err != nil {
		return fmt.Errorf("resume decided run %s: %w", cp.RunID, err)
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
// The claim is pinned to bound, the pause the checkpoint was opened for,
// on a store that implements suspension.VersionedStore (every built-in
// one). A stale rejection whose run has since paused again on another
// suspension is refused with cortex.ErrInvalidState and the run is left
// paused: it would otherwise fail a run for a pause nobody rejected. A
// store without the capability claims whatever pause the run has, after
// the same check made just before the claim.
//
// Once the claim succeeds the run belongs to this call, exactly as it
// does in Resume, so every exit below either fails the run or reports
// why it could not. The suspension is deleted after the state write and
// never before: the row is the only record of what the run was waiting
// on, and a delete that landed while the state write failed would throw
// that away for nothing.
func (e *Engine) failRejected(ctx context.Context, cp *checkpoint.Checkpoint, bound *suspension.Suspension, decision checkpoint.Decision) error {
	// Read before the claim, for the reason claimedRun spells out: a
	// read that fails here costs nothing, because the run is still
	// paused and the decider can decide again, while the same failure
	// after the claim would leave the run running with nothing able to
	// take it.
	snapshot, err := e.store.GetRun(ctx, cp.RunID)
	if err != nil {
		return fmt.Errorf("load run for rejected checkpoint: %w", err)
	}

	susp, err := e.claimRejected(ctx, cp, bound, snapshot)
	if err != nil {
		return fmt.Errorf("claim suspension for rejected checkpoint %s: %w", cp.ID, err)
	}

	// Write under the run's own scope, not the decider's. A checkpoint is
	// resolvable by anyone whose scope covers the run, and everything the
	// run records has to land where its earlier writes did.
	ctx = cortex.WithScope(ctx, susp.Scope)

	r := e.claimedRun(ctx, cp.RunID, snapshot, susp.ClaimedRun)

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
		e.logError("delete suspension of a rejected run", err, log.String("run_id", cp.RunID.String()))
	}
	return nil
}

// claimRejected claims the pause a rejection answers. With a versioned
// store the claim takes bound and nothing else; a run waiting on another
// pause is cortex.ErrInvalidState.
func (e *Engine) claimRejected(ctx context.Context, cp *checkpoint.Checkpoint, bound *suspension.Suspension, snapshot *run.Run) (*suspension.Suspension, error) {
	// A claim that takes the lease leaves a lease that lapses if the
	// failure below is never written, and the run is reclaimed from the
	// state it paused with: the person is asked again.
	if bound != nil {
		if susp, leased, err := e.claimPauseLeased(ctx, snapshot, bound, time.Time{}, claimSnapshot(cp.RunID, bound.Cont, nil)); leased {
			return e.rejectedClaimResult(ctx, cp, bound, susp, err)
		}
	}
	versioned, ok := e.store.(suspension.VersionedStore)
	if !ok || bound == nil {
		return e.store.ClaimSuspension(ctx, cp.RunID)
	}
	susp, err := versioned.ClaimSuspensionVersion(ctx, suspension.ClaimVersion{RunID: cp.RunID, SuspensionID: bound.ID, RunRevision: snapshot.Revision})
	if errors.Is(err, suspension.ErrVersionClaimUnavailable) {
		// A store that implements the interface but cannot pin the pause
		// right now is treated like one without it.
		return e.store.ClaimSuspension(ctx, cp.RunID)
	}
	return e.rejectedClaimResult(ctx, cp, bound, susp, err)
}

// rejectedClaimResult maps a pinned rejection claim's refusal: a run
// waiting on another pause is cortex.ErrInvalidState.
func (e *Engine) rejectedClaimResult(ctx context.Context, cp *checkpoint.Checkpoint, bound, susp *suspension.Suspension, err error) (*suspension.Suspension, error) {
	if err == nil || (!errors.Is(err, cortex.ErrNotSuspended) && !errors.Is(err, cortex.ErrRunConflict)) {
		return susp, err
	}
	if current, readErr := e.store.GetSuspension(ctx, cp.RunID); readErr == nil && current.ID != bound.ID {
		return nil, fmt.Errorf("the run is no longer waiting on the pause this checkpoint was opened for: %w", cortex.ErrInvalidState)
	}
	return nil, err
}

// CancelRun ends a run at an operator's request, and it is the only way
// a host should end one: a plain UpdateRun to cancelled is the fourth
// writer to a paused run, and the other three go through a claim for a
// reason.
//
// A paused run is cancelled through ClaimSuspension, the same atomic
// paused-to-running transition a resume, a rejected checkpoint and the
// expiry sweep all take. Without it, cancel read a paused run and wrote
// unconditionally, so a resume that had already claimed the run got its
// state stomped to cancelled and then wrote its own terminal state back
// over the cancel a moment later. The operator was told the run was
// cancelled and the run went on to complete. Losing the claim is
// reported as cortex.ErrNotSuspended rather than worked around, because
// somebody else is already moving this run.
//
// The claim also hands back the row, which is what lets cancel clean up
// after itself: the suspension is dropped, and any checkpoint the pause
// opened is resolved, so nobody is left holding a decision on a run that
// has ended.
//
// A ready or reclaimable run is claimed with ClaimContinuation, the same
// transition ContinueRun takes, and then ended exactly like a paused one.
//
// A run that is merely running is cancelled as before, with no claim to
// take: there is no suspension behind it, and a running run has no
// engine-side transition to race with that a store predicate could
// settle.
func (e *Engine) CancelRun(ctx context.Context, runID id.AgentRunID) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}

	r, err := e.store.GetRun(ctx, runID)
	if err != nil {
		return fmt.Errorf("load run to cancel: %w", err)
	}
	switch r.State {
	case run.StateRunning:
	case run.StatePaused:
		return e.cancelPaused(ctx, r)
	case run.StateReady, run.StateReclaimable:
		return e.cancelContinuable(ctx, r)
	default:
		return fmt.Errorf("cancel run %s: run is %s, which is not cancellable: %w", runID, r.State, cortex.ErrInvalidState)
	}

	r.State = run.StateCancelled
	r.StopReason = run.StopCancelled
	completedAt := time.Now().UTC()
	r.CompletedAt = &completedAt
	if err := e.persistRun(ctx, r); err != nil {
		return fmt.Errorf("cancel run %s: %w", runID, err)
	}
	e.emitTerminalOutcome(context.WithoutCancel(ctx), r)
	return nil
}

// cancelPaused is the claimed half of CancelRun for a paused run. Once the
// claim succeeds the run belongs to this call and is no longer paused, so
// every exit below either ends it or says why it could not, exactly as
// the resume and rejection paths do.
func (e *Engine) cancelPaused(ctx context.Context, r *run.Run) error {
	susp, err := e.claimToEnd(ctx, r)
	if err != nil {
		return fmt.Errorf("claim suspension to cancel run %s: %w", r.ID, err)
	}

	// Under the run's own scope, not the operator's, for the same reason
	// every other writer here rebinds: a canceller whose scope is one
	// level broader than the run still writes where the run's earlier
	// writes landed.
	ctx = cortex.WithScope(ctx, susp.Scope)

	// The run already read above is the snapshot, so a read that fails
	// after the claim cannot leave the run running with nothing able to
	// take it again.
	return e.cancelClaimed(ctx, e.claimedRun(ctx, r.ID, r, susp.ClaimedRun))
}

// claimToEnd claims a paused run that this call is about to end. Under
// the lease when this engine can take it with the claim, so a canceller
// that dies before its write leaves a lease that lapses, not a running run
// nobody owns; the run is then reclaimed from the state it paused with.
func (e *Engine) claimToEnd(ctx context.Context, r *run.Run) (*suspension.Suspension, error) {
	if _, leased := e.leasedClaimsFor(r); leased {
		paused, err := e.store.GetSuspension(ctx, r.ID)
		if err != nil {
			return nil, err
		}
		susp, _, err := e.claimPauseLeased(ctx, r, paused, time.Time{}, claimSnapshot(r.ID, paused.Cont, nil))
		if errors.Is(err, cortex.ErrRunConflict) {
			// The run moved since it was read: somebody else claimed it.
			return nil, fmt.Errorf("%w: %w", cortex.ErrNotSuspended, err)
		}
		return susp, err
	}
	return e.store.ClaimSuspension(ctx, r.ID)
}

// claimContinuableToEnd claims a ready or reclaimable run that this call is
// about to end, under this worker's lease when it leases the run, for the
// same reason as claimToEnd.
func (e *Engine) claimContinuableToEnd(ctx context.Context, r *run.Run) (*run.Run, error) {
	if claims, leased := e.leasedClaimsFor(r); leased {
		return claims.ClaimContinuationLeased(ctx, r.ID, e.newLease(), nil)
	}
	return e.continuation.ClaimContinuation(ctx, r.ID, run.Lease{})
}

// cancelContinuable is the claimed half of CancelRun for a ready or
// reclaimable run. ClaimContinuation takes it exactly as a ContinueRun
// would, so a cancel and a continuation racing for the same run cannot
// both win, and the cancel then ends it through the same path a paused
// run takes.
func (e *Engine) cancelContinuable(ctx context.Context, r *run.Run) error {
	if e.continuation == nil {
		return fmt.Errorf("cancel run %s: %w", r.ID, cortex.ErrContinuationUnavailable)
	}
	claimed, err := e.claimContinuableToEnd(ctx, r)
	if err != nil {
		return fmt.Errorf("claim run %s to cancel: %w", r.ID, err)
	}
	return e.cancelClaimed(cortex.WithScope(ctx, claimed.Scope), claimed)
}

// cancelClaimed ends a run this call claimed, then drops what it was
// waiting with: the suspension, any progress snapshot, and the checkpoints
// it left in the decision queue.
func (e *Engine) cancelClaimed(ctx context.Context, r *run.Run) error {
	r.State = run.StateCancelled
	r.StopReason = run.StopCancelled
	if err := e.persistRun(context.WithoutCancel(ctx), r); err != nil {
		// The suspension stays. It is the only record of what the run was
		// waiting on, and the run is still running from the claim, so
		// dropping the row now would throw that away for nothing.
		return fmt.Errorf("cancel run %s: %w", r.ID, err)
	}

	e.emitTerminalOutcome(context.WithoutCancel(ctx), r)

	// The run is cancelled and no claim will ever take it again, so the
	// row is dead weight, and dropping it is what makes the cancel final.
	// Its progress snapshot went with the terminal hook above.
	if err := e.store.DeleteSuspension(ctx, r.ID); err != nil && !errors.Is(err, cortex.ErrNotSuspended) {
		e.logError("delete suspension of a cancelled run", err, log.String("run_id", r.ID.String()))
	}
	e.closePendingCheckpoints(ctx, r.ID, "the run was cancelled, so there is nothing left to decide")
	return nil
}

// closePendingCheckpoints resolves whatever a run that has ended left in
// the decision queue, so nobody is asked to approve a run nothing can
// act on. reason is what the operator reads on the row, and it differs
// by how the run ended.
//
// Called once the run's terminal state is written and never before. A
// checkpoint resolved against a run that then failed to move would be
// the audit lie ResolveCheckpoint's ordering exists to avoid, reached
// from the other side.
//
// It is best effort and logged rather than returned: the run really did
// end, which is what the caller cares about, and a row left in a queue
// is not worth reporting that as a failure. checkpoint.Store has no
// resolve-by-run-id, so the rows are found through the pending list,
// which does filter by run.
func (e *Engine) closePendingCheckpoints(ctx context.Context, runID id.AgentRunID, reason string) {
	cps, err := e.store.ListPending(ctx, &checkpoint.ListFilter{RunID: runID.String()})
	if err != nil {
		e.logError("list the checkpoints of a run that ended", err, log.String("run_id", runID.String()))
		return
	}
	decision := checkpoint.Decision{
		DecidedBy: checkpointDecidedByEngine,
		Reason:    reason,
		DecidedAt: time.Now().UTC(),
	}
	for _, cp := range cps {
		if err := e.store.Resolve(ctx, cp.ID, decision); err != nil && !errors.Is(err, cortex.ErrInvalidState) {
			e.logError("resolve the checkpoint of a run that ended", err,
				log.String("checkpoint_id", cp.ID.String()),
				log.String("run_id", runID.String()),
			)
		}
	}
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
	EventIncomplete  StreamEventType = "incomplete"
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
//
// The run is traced as one run agent span covering the whole call.
func (e *Engine) RunAgent(ctx context.Context, agentName, input string, overrides *RunOverrides) (r *run.Run, err error) {
	ctx, done, err := e.admit(ctx)
	if err != nil {
		return nil, err
	}
	defer done()
	ctx, rt := e.startRun(ctx, modeSync, false)
	defer func() {
		if p := recover(); p != nil {
			rt.end(r, telemetry.ErrPanic)
			panic(p)
		}
		rt.end(r, err)
	}()
	return e.runAgent(ctx, agentName, input, overrides)
}

func (e *Engine) runAgent(ctx context.Context, agentName, input string, overrides *RunOverrides) (*run.Run, error) {
	if e.store == nil {
		return nil, cortex.ErrNoStore
	}
	if _, scopeErr := cortex.RequireScope(ctx); scopeErr != nil {
		return nil, scopeErr
	}

	ag, err := e.store.GetByName(ctx, agentName)
	if err != nil {
		return nil, fmt.Errorf("resolve agent: %w", err)
	}

	if scopeErr := requireAgentScope(ctx, ag); scopeErr != nil {
		return nil, scopeErr
	}

	// Use real execution if LLM client is available.
	if e.llm != nil {
		return e.runReAct(ctx, ag, input, overrides)
	}

	// Fallback: mock/echo execution.
	return e.runMock(ctx, ag, input, overrides)
}

// StreamAgent executes an agent and sends streaming events to the channel.
// The channel is closed when execution completes.
// When an LLM client is configured, it uses the ReAct reasoning loop with streaming.
// Otherwise, it falls back to mock/echo mode.
//
// The run agent span, and the run's admission slot, last until the stream's
// goroutine finishes, not until this call returns.
func (e *Engine) StreamAgent(ctx context.Context, agentName, input string, overrides *RunOverrides, events chan<- StreamEvent) (err error) {
	ctx, done, err := e.admit(ctx)
	if err != nil {
		close(events)
		return err
	}
	ctx, rt := e.startRun(ctx, modeStream, false)
	defer func() {
		if p := recover(); p != nil {
			done()
			rt.end(nil, telemetry.ErrPanic)
			panic(p)
		}
		if err != nil {
			done()
			rt.end(nil, err)
		}
	}()
	return e.streamAgent(ctx, agentName, input, overrides, events, done)
}

// streamAgent hands done to the stream's goroutine, which calls it when it
// closes events. On an error return the goroutine never started and the
// caller still owns done.
func (e *Engine) streamAgent(ctx context.Context, agentName, input string, overrides *RunOverrides, events chan<- StreamEvent, done func()) error {
	if e.store == nil {
		close(events)
		return cortex.ErrNoStore
	}
	if _, scopeErr := cortex.RequireScope(ctx); scopeErr != nil {
		close(events)
		return scopeErr
	}

	ag, err := e.store.GetByName(ctx, agentName)
	if err != nil {
		close(events)
		return fmt.Errorf("resolve agent: %w", err)
	}

	if scopeErr := requireAgentScope(ctx, ag); scopeErr != nil {
		close(events)
		return scopeErr
	}

	// Use real execution if LLM client is available.
	if e.llm != nil {
		return e.streamReAct(ctx, ag, input, overrides, events, done)
	}

	// Fallback: mock/echo execution.
	return e.streamMock(ctx, ag, input, overrides, events, done)
}

// mockRunID is the id a mock run is created with: the override's, or a
// new one.
func mockRunID(overrides *RunOverrides) (id.AgentRunID, error) {
	if overrides == nil || overrides.RunID.IsNil() {
		return id.NewAgentRunID(), nil
	}
	if overrides.RunID.Prefix() != id.PrefixAgentRun {
		return id.Nil, fmt.Errorf("cortex: run id %s is not a run id: %w", overrides.RunID, cortex.ErrInvalidPrepared)
	}
	return overrides.RunID, nil
}

// createMockRun writes a mock run, under this worker's lease when the
// engine leases runs, and starts renewing it. A mock run saves no
// progress: there is no model state to continue from.
func (e *Engine) createMockRun(ctx context.Context, r *run.Run) (context.Context, error) {
	leased := e.leasesRun(r)
	if leased {
		lease := e.newLease()
		r.Lease = &lease
	}
	if err := e.store.CreateRun(ctx, r); err != nil {
		return ctx, fmt.Errorf("create run: %w", err)
	}
	if leased {
		ctx = e.holdLease(ctx, r)
	}
	return ctx, nil
}

// runMock is the mock/echo fallback for RunAgent.
func (e *Engine) runMock(ctx context.Context, ag *agent.Config, input string, overrides *RunOverrides) (*run.Run, error) {
	principal, principalErr := e.encodePrincipal(ctx)
	if principalErr != nil {
		return nil, principalErr
	}
	runID, idErr := mockRunID(overrides)
	if idErr != nil {
		return nil, idErr
	}
	now := time.Now().UTC()
	r := &run.Run{
		Entity:     cortex.NewEntity(),
		ID:         runID,
		AgentID:    ag.ID,
		State:      run.StateRunning,
		Input:      input,
		StartedAt:  &now,
		PersonaRef: ag.PersonaRef,
		Principal:  principal,
	}
	safeInput, scanErr := e.checkPublication(ctx, ag, r, e.frozenPublication(), input, "user_input", safety.DirectionInput)
	if scanErr != nil {
		return nil, scanErr
	}
	input = string(safeInput)
	r.Input = input
	ctx, createErr := e.createMockRun(ctx, r)
	if createErr != nil {
		return nil, createErr
	}
	defer releaseLease(ctx, r.ID)
	runTraceFrom(ctx).bind(r, "")

	e.extensions.EmitRunStarted(ctx, ag.ID, r.ID, input)

	checked, scanErr := e.checkPublication(ctx, ag, r, e.frozenPublication(), "Echo: "+input, "model_output", safety.DirectionOutput)
	if scanErr != nil {
		return r, e.stopPublication(ctx, r, &reactState{}, scanErr)
	}
	stepStart := time.Now().UTC()
	step := &run.Step{
		Entity:     cortex.NewEntity(),
		ID:         id.NewStepID(),
		RunID:      r.ID,
		Index:      0,
		Type:       "generation",
		Input:      input,
		Output:     string(checked),
		TokensUsed: len(input) + 6,
		StartedAt:  &stepStart,
	}
	stepEnd := time.Now().UTC()
	step.CompletedAt = &stepEnd
	if err := e.store.CreateStep(ctx, step); err != nil {
		e.logError("create step", err, log.String("run_id", r.ID.String()))
	}

	if err := e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopFinalAnswer, Output: step.Output, StepCount: 1, TokensUsed: step.TokensUsed}); err != nil {
		return r, err
	}

	return r, nil
}

// streamMock is the mock/echo fallback for StreamAgent.
func (e *Engine) streamMock(ctx context.Context, ag *agent.Config, input string, overrides *RunOverrides, events chan<- StreamEvent, done func()) error {
	principal, principalErr := e.encodePrincipal(ctx)
	if principalErr != nil {
		close(events)
		return principalErr
	}
	runID, idErr := mockRunID(overrides)
	if idErr != nil {
		close(events)
		return idErr
	}
	now := time.Now().UTC()
	r := &run.Run{
		Entity:     cortex.NewEntity(),
		ID:         runID,
		AgentID:    ag.ID,
		State:      run.StateRunning,
		Input:      input,
		StartedAt:  &now,
		PersonaRef: ag.PersonaRef,
		Principal:  principal,
	}
	safeInput, scanErr := e.checkPublication(ctx, ag, r, e.frozenPublication(), input, "user_input", safety.DirectionInput)
	if scanErr != nil {
		close(events)
		return scanErr
	}
	input = string(safeInput)
	r.Input = input
	ctx, createErr := e.createMockRun(ctx, r)
	if createErr != nil {
		close(events)
		return createErr
	}
	rt := runTraceFrom(ctx)
	rt.bind(r, "")

	e.extensions.EmitRunStarted(ctx, ag.ID, r.ID, input)

	go func() {
		defer close(events)
		defer done()
		defer releaseLease(ctx, r.ID)
		defer func() {
			if p := recover(); p != nil {
				rt.end(r, telemetry.ErrPanic)
				panic(p)
			}
			rt.end(r, nil)
		}()

		sendEvent(ctx, events, StreamEvent{Type: EventRunStarted, Data: map[string]any{
			"run_id":   r.ID.String(),
			"agent_id": ag.ID.String(),
		}})

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

		sendEvent(ctx, events, StreamEvent{Type: EventStep, Data: map[string]any{
			"step_id": step.ID.String(),
			"index":   0,
			"type":    "generation",
		}})

		checked, scanErr := e.checkPublication(ctx, ag, r, e.frozenPublication(), "Echo: "+input, "model_output", safety.DirectionOutput)
		if scanErr != nil {
			terminalErr := e.stopPublication(ctx, r, &reactState{}, scanErr)
			sendEvent(ctx, events, StreamEvent{Type: EventSafetyBlock, Data: map[string]any{"message": terminalErr.Error()}})
			return
		}
		response := string(checked)
		for i, ch := range response {
			select {
			case <-ctx.Done():
				terminalErr := e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopCancelled})
				message := ctx.Err().Error()
				if terminalErr != nil {
					message = terminalErr.Error()
				}
				sendEvent(ctx, events, StreamEvent{Type: EventError, Data: map[string]any{"message": message}})

				return
			default:
			}

			sendEvent(ctx, events, StreamEvent{Type: EventToken, Data: map[string]any{
				"content": string(ch),
				"index":   i,
			}})

			time.Sleep(20 * time.Millisecond)
		}

		stepEnd := time.Now().UTC()
		step.Output = response
		step.TokensUsed = len(input) + 6
		step.CompletedAt = &stepEnd
		if err := e.store.CreateStep(ctx, step); err != nil {
			e.logError("create step", err, log.String("run_id", r.ID.String()))
		}

		if err := e.finalizeRun(ctx, r, run.Outcome{StopReason: run.StopFinalAnswer, Output: response, StepCount: 1, TokensUsed: step.TokensUsed}); err != nil {
			sendEvent(ctx, events, StreamEvent{Type: EventError, Data: map[string]any{"message": err.Error()}})
			return
		}
		sendEvent(ctx, events, outcomeEvent(r, time.Since(now)))
	}()

	return nil
}

// CreateRun creates a run record directly (for external use). A run that
// carries no principal bytes gets the context's, encoded the same way the
// engine's own runs are.
func (e *Engine) CreateRun(ctx context.Context, r *run.Run) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	if r.Principal == nil {
		principal, err := e.encodePrincipal(ctx)
		if err != nil {
			return err
		}
		r.Principal = principal
	}
	return e.store.CreateRun(ctx, r)
}

// UpdateRun updates a run record (for external use like cancellation).
func (e *Engine) UpdateRun(ctx context.Context, r *run.Run) error {
	if e.store == nil {
		return cortex.ErrNoStore
	}
	return e.persistRun(ctx, r)
}
