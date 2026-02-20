package plugin

import (
	"context"
	"log/slog"
	"time"

	"github.com/xraph/cortex/id"
)

// Named entry types pair a hook implementation with the extension name
// captured at registration time.
type runStartedEntry struct {
	name string
	hook RunStarted
}

type runCompletedEntry struct {
	name string
	hook RunCompleted
}

type runFailedEntry struct {
	name string
	hook RunFailed
}

type stepStartedEntry struct {
	name string
	hook StepStarted
}

type stepCompletedEntry struct {
	name string
	hook StepCompleted
}

type toolCalledEntry struct {
	name string
	hook ToolCalled
}

type toolCompletedEntry struct {
	name string
	hook ToolCompleted
}

type toolFailedEntry struct {
	name string
	hook ToolFailed
}

type personaResolvedEntry struct {
	name string
	hook PersonaResolved
}

type behaviorTriggeredEntry struct {
	name string
	hook BehaviorTriggered
}

type cognitivePhaseChangedEntry struct {
	name string
	hook CognitivePhaseChanged
}

type checkpointCreatedEntry struct {
	name string
	hook CheckpointCreated
}

type checkpointResolvedEntry struct {
	name string
	hook CheckpointResolved
}

type orchestrationStartedEntry struct {
	name string
	hook OrchestrationStarted
}

type orchestrationCompletedEntry struct {
	name string
	hook OrchestrationCompleted
}

type agentHandoffEntry struct {
	name string
	hook AgentHandoff
}

type shutdownEntry struct {
	name string
	hook Shutdown
}

// Registry holds registered extensions and dispatches lifecycle events.
// It type-caches extensions at registration time so emit calls iterate
// only over extensions that implement the relevant hook.
type Registry struct {
	extensions []Extension
	logger     *slog.Logger

	runStarted             []runStartedEntry
	runCompleted           []runCompletedEntry
	runFailed              []runFailedEntry
	stepStarted            []stepStartedEntry
	stepCompleted          []stepCompletedEntry
	toolCalled             []toolCalledEntry
	toolCompleted          []toolCompletedEntry
	toolFailed             []toolFailedEntry
	personaResolved        []personaResolvedEntry
	behaviorTriggered      []behaviorTriggeredEntry
	cognitivePhaseChanged  []cognitivePhaseChangedEntry
	checkpointCreated      []checkpointCreatedEntry
	checkpointResolved     []checkpointResolvedEntry
	orchestrationStarted   []orchestrationStartedEntry
	orchestrationCompleted []orchestrationCompletedEntry
	agentHandoff           []agentHandoffEntry
	shutdown               []shutdownEntry
}

// NewRegistry creates an extension registry with the given logger.
func NewRegistry(logger *slog.Logger) *Registry {
	return &Registry{logger: logger}
}

// Register adds an extension and type-asserts it into all applicable
// hook caches. Extensions are notified in registration order.
func (r *Registry) Register(e Extension) {
	r.extensions = append(r.extensions, e)
	name := e.Name()

	if h, ok := e.(RunStarted); ok {
		r.runStarted = append(r.runStarted, runStartedEntry{name, h})
	}
	if h, ok := e.(RunCompleted); ok {
		r.runCompleted = append(r.runCompleted, runCompletedEntry{name, h})
	}
	if h, ok := e.(RunFailed); ok {
		r.runFailed = append(r.runFailed, runFailedEntry{name, h})
	}
	if h, ok := e.(StepStarted); ok {
		r.stepStarted = append(r.stepStarted, stepStartedEntry{name, h})
	}
	if h, ok := e.(StepCompleted); ok {
		r.stepCompleted = append(r.stepCompleted, stepCompletedEntry{name, h})
	}
	if h, ok := e.(ToolCalled); ok {
		r.toolCalled = append(r.toolCalled, toolCalledEntry{name, h})
	}
	if h, ok := e.(ToolCompleted); ok {
		r.toolCompleted = append(r.toolCompleted, toolCompletedEntry{name, h})
	}
	if h, ok := e.(ToolFailed); ok {
		r.toolFailed = append(r.toolFailed, toolFailedEntry{name, h})
	}
	if h, ok := e.(PersonaResolved); ok {
		r.personaResolved = append(r.personaResolved, personaResolvedEntry{name, h})
	}
	if h, ok := e.(BehaviorTriggered); ok {
		r.behaviorTriggered = append(r.behaviorTriggered, behaviorTriggeredEntry{name, h})
	}
	if h, ok := e.(CognitivePhaseChanged); ok {
		r.cognitivePhaseChanged = append(r.cognitivePhaseChanged, cognitivePhaseChangedEntry{name, h})
	}
	if h, ok := e.(CheckpointCreated); ok {
		r.checkpointCreated = append(r.checkpointCreated, checkpointCreatedEntry{name, h})
	}
	if h, ok := e.(CheckpointResolved); ok {
		r.checkpointResolved = append(r.checkpointResolved, checkpointResolvedEntry{name, h})
	}
	if h, ok := e.(OrchestrationStarted); ok {
		r.orchestrationStarted = append(r.orchestrationStarted, orchestrationStartedEntry{name, h})
	}
	if h, ok := e.(OrchestrationCompleted); ok {
		r.orchestrationCompleted = append(r.orchestrationCompleted, orchestrationCompletedEntry{name, h})
	}
	if h, ok := e.(AgentHandoff); ok {
		r.agentHandoff = append(r.agentHandoff, agentHandoffEntry{name, h})
	}
	if h, ok := e.(Shutdown); ok {
		r.shutdown = append(r.shutdown, shutdownEntry{name, h})
	}
}

// Extensions returns all registered extensions.
func (r *Registry) Extensions() []Extension { return r.extensions }

// ──────────────────────────────────────────────────
// Run event emitters
// ──────────────────────────────────────────────────

func (r *Registry) EmitRunStarted(ctx context.Context, agentID id.AgentID, runID id.AgentRunID, input string) {
	for _, e := range r.runStarted {
		if err := e.hook.OnRunStarted(ctx, agentID, runID, input); err != nil {
			r.logHookError("OnRunStarted", e.name, err)
		}
	}
}

func (r *Registry) EmitRunCompleted(ctx context.Context, agentID id.AgentID, runID id.AgentRunID, output string, elapsed time.Duration) {
	for _, e := range r.runCompleted {
		if err := e.hook.OnRunCompleted(ctx, agentID, runID, output, elapsed); err != nil {
			r.logHookError("OnRunCompleted", e.name, err)
		}
	}
}

func (r *Registry) EmitRunFailed(ctx context.Context, agentID id.AgentID, runID id.AgentRunID, runErr error) {
	for _, e := range r.runFailed {
		if err := e.hook.OnRunFailed(ctx, agentID, runID, runErr); err != nil {
			r.logHookError("OnRunFailed", e.name, err)
		}
	}
}

// ──────────────────────────────────────────────────
// Step event emitters
// ──────────────────────────────────────────────────

func (r *Registry) EmitStepStarted(ctx context.Context, runID id.AgentRunID, stepIndex int) {
	for _, e := range r.stepStarted {
		if err := e.hook.OnStepStarted(ctx, runID, stepIndex); err != nil {
			r.logHookError("OnStepStarted", e.name, err)
		}
	}
}

func (r *Registry) EmitStepCompleted(ctx context.Context, runID id.AgentRunID, stepIndex int, elapsed time.Duration) {
	for _, e := range r.stepCompleted {
		if err := e.hook.OnStepCompleted(ctx, runID, stepIndex, elapsed); err != nil {
			r.logHookError("OnStepCompleted", e.name, err)
		}
	}
}

// ──────────────────────────────────────────────────
// Tool event emitters
// ──────────────────────────────────────────────────

func (r *Registry) EmitToolCalled(ctx context.Context, runID id.AgentRunID, toolName string, args any) {
	for _, e := range r.toolCalled {
		if err := e.hook.OnToolCalled(ctx, runID, toolName, args); err != nil {
			r.logHookError("OnToolCalled", e.name, err)
		}
	}
}

func (r *Registry) EmitToolCompleted(ctx context.Context, runID id.AgentRunID, toolName string, result string, elapsed time.Duration) {
	for _, e := range r.toolCompleted {
		if err := e.hook.OnToolCompleted(ctx, runID, toolName, result, elapsed); err != nil {
			r.logHookError("OnToolCompleted", e.name, err)
		}
	}
}

func (r *Registry) EmitToolFailed(ctx context.Context, runID id.AgentRunID, toolName string, toolErr error) {
	for _, e := range r.toolFailed {
		if err := e.hook.OnToolFailed(ctx, runID, toolName, toolErr); err != nil {
			r.logHookError("OnToolFailed", e.name, err)
		}
	}
}

// ──────────────────────────────────────────────────
// Persona event emitters
// ──────────────────────────────────────────────────

func (r *Registry) EmitPersonaResolved(ctx context.Context, agentID id.AgentID, personaName string) {
	for _, e := range r.personaResolved {
		if err := e.hook.OnPersonaResolved(ctx, agentID, personaName); err != nil {
			r.logHookError("OnPersonaResolved", e.name, err)
		}
	}
}

func (r *Registry) EmitBehaviorTriggered(ctx context.Context, runID id.AgentRunID, behaviorName string) {
	for _, e := range r.behaviorTriggered {
		if err := e.hook.OnBehaviorTriggered(ctx, runID, behaviorName); err != nil {
			r.logHookError("OnBehaviorTriggered", e.name, err)
		}
	}
}

func (r *Registry) EmitCognitivePhaseChanged(ctx context.Context, runID id.AgentRunID, fromPhase, toPhase string) {
	for _, e := range r.cognitivePhaseChanged {
		if err := e.hook.OnCognitivePhaseChanged(ctx, runID, fromPhase, toPhase); err != nil {
			r.logHookError("OnCognitivePhaseChanged", e.name, err)
		}
	}
}

// ──────────────────────────────────────────────────
// Checkpoint event emitters
// ──────────────────────────────────────────────────

func (r *Registry) EmitCheckpointCreated(ctx context.Context, cpID id.CheckpointID, runID id.AgentRunID, reason string) {
	for _, e := range r.checkpointCreated {
		if err := e.hook.OnCheckpointCreated(ctx, cpID, runID, reason); err != nil {
			r.logHookError("OnCheckpointCreated", e.name, err)
		}
	}
}

func (r *Registry) EmitCheckpointResolved(ctx context.Context, cpID id.CheckpointID, decision string) {
	for _, e := range r.checkpointResolved {
		if err := e.hook.OnCheckpointResolved(ctx, cpID, decision); err != nil {
			r.logHookError("OnCheckpointResolved", e.name, err)
		}
	}
}

// ──────────────────────────────────────────────────
// Orchestration event emitters
// ──────────────────────────────────────────────────

func (r *Registry) EmitOrchestrationStarted(ctx context.Context, orchID id.OrchestrationID, strategy string) {
	for _, e := range r.orchestrationStarted {
		if err := e.hook.OnOrchestrationStarted(ctx, orchID, strategy); err != nil {
			r.logHookError("OnOrchestrationStarted", e.name, err)
		}
	}
}

func (r *Registry) EmitOrchestrationCompleted(ctx context.Context, orchID id.OrchestrationID, elapsed time.Duration) {
	for _, e := range r.orchestrationCompleted {
		if err := e.hook.OnOrchestrationCompleted(ctx, orchID, elapsed); err != nil {
			r.logHookError("OnOrchestrationCompleted", e.name, err)
		}
	}
}

func (r *Registry) EmitAgentHandoff(ctx context.Context, orchID id.OrchestrationID, fromAgent, toAgent string, payload string) {
	for _, e := range r.agentHandoff {
		if err := e.hook.OnAgentHandoff(ctx, orchID, fromAgent, toAgent, payload); err != nil {
			r.logHookError("OnAgentHandoff", e.name, err)
		}
	}
}

// ──────────────────────────────────────────────────
// Shutdown event emitter
// ──────────────────────────────────────────────────

func (r *Registry) EmitShutdown(ctx context.Context) {
	for _, e := range r.shutdown {
		if err := e.hook.OnShutdown(ctx); err != nil {
			r.logHookError("OnShutdown", e.name, err)
		}
	}
}

// logHookError logs a warning when a lifecycle hook returns an error.
// Errors from hooks are never propagated — they must not block the pipeline.
func (r *Registry) logHookError(hook, extName string, err error) {
	r.logger.Warn("extension hook error",
		slog.String("hook", hook),
		slog.String("extension", extName),
		slog.String("error", err.Error()),
	)
}
