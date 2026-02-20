// Package plugin defines the extension system for Cortex.
//
// Extensions are notified of lifecycle events (run started, tool called,
// persona resolved, etc.) and can react to them — logging, metrics,
// tracing, auditing, etc.
//
// Each lifecycle hook is a separate interface so extensions opt in only
// to the events they care about.
package plugin

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// ──────────────────────────────────────────────────
// Base extension interface
// ──────────────────────────────────────────────────

// Extension is the base interface all Cortex plugins must implement.
type Extension interface {
	Name() string
}

// ──────────────────────────────────────────────────
// Agent lifecycle hooks
// ──────────────────────────────────────────────────

// RunStarted is called when an agent run begins.
type RunStarted interface {
	OnRunStarted(ctx context.Context, agentID id.AgentID, runID id.AgentRunID, input string) error
}

// RunCompleted is called when an agent run finishes successfully.
type RunCompleted interface {
	OnRunCompleted(ctx context.Context, agentID id.AgentID, runID id.AgentRunID, output string, elapsed time.Duration) error
}

// RunFailed is called when an agent run fails.
type RunFailed interface {
	OnRunFailed(ctx context.Context, agentID id.AgentID, runID id.AgentRunID, err error) error
}

// ──────────────────────────────────────────────────
// Reasoning lifecycle hooks
// ──────────────────────────────────────────────────

// StepStarted is called when a reasoning step begins.
type StepStarted interface {
	OnStepStarted(ctx context.Context, runID id.AgentRunID, stepIndex int) error
}

// StepCompleted is called when a reasoning step finishes.
type StepCompleted interface {
	OnStepCompleted(ctx context.Context, runID id.AgentRunID, stepIndex int, elapsed time.Duration) error
}

// ──────────────────────────────────────────────────
// Tool lifecycle hooks
// ──────────────────────────────────────────────────

// ToolCalled is called when a tool invocation begins.
type ToolCalled interface {
	OnToolCalled(ctx context.Context, runID id.AgentRunID, toolName string, args any) error
}

// ToolCompleted is called when a tool invocation finishes successfully.
type ToolCompleted interface {
	OnToolCompleted(ctx context.Context, runID id.AgentRunID, toolName string, result string, elapsed time.Duration) error
}

// ToolFailed is called when a tool invocation fails.
type ToolFailed interface {
	OnToolFailed(ctx context.Context, runID id.AgentRunID, toolName string, err error) error
}

// ──────────────────────────────────────────────────
// Persona lifecycle hooks
// ──────────────────────────────────────────────────

// PersonaResolved is called when a persona is resolved for a run.
type PersonaResolved interface {
	OnPersonaResolved(ctx context.Context, agentID id.AgentID, personaName string) error
}

// BehaviorTriggered is called when a behavior fires during a run.
type BehaviorTriggered interface {
	OnBehaviorTriggered(ctx context.Context, runID id.AgentRunID, behaviorName string) error
}

// CognitivePhaseChanged is called when the cognitive engine switches phases.
type CognitivePhaseChanged interface {
	OnCognitivePhaseChanged(ctx context.Context, runID id.AgentRunID, fromPhase, toPhase string) error
}

// ──────────────────────────────────────────────────
// Checkpoint lifecycle hooks
// ──────────────────────────────────────────────────

// CheckpointCreated is called when a checkpoint is created.
type CheckpointCreated interface {
	OnCheckpointCreated(ctx context.Context, cpID id.CheckpointID, runID id.AgentRunID, reason string) error
}

// CheckpointResolved is called when a checkpoint is resolved.
type CheckpointResolved interface {
	OnCheckpointResolved(ctx context.Context, cpID id.CheckpointID, decision string) error
}

// ──────────────────────────────────────────────────
// Orchestration lifecycle hooks
// ──────────────────────────────────────────────────

// OrchestrationStarted is called when multi-agent orchestration begins.
type OrchestrationStarted interface {
	OnOrchestrationStarted(ctx context.Context, orchID id.OrchestrationID, strategy string) error
}

// OrchestrationCompleted is called when orchestration finishes.
type OrchestrationCompleted interface {
	OnOrchestrationCompleted(ctx context.Context, orchID id.OrchestrationID, elapsed time.Duration) error
}

// AgentHandoff is called when one agent hands off to another.
type AgentHandoff interface {
	OnAgentHandoff(ctx context.Context, orchID id.OrchestrationID, fromAgent, toAgent string, payload string) error
}

// ──────────────────────────────────────────────────
// Shutdown hook
// ──────────────────────────────────────────────────

// Shutdown is called during graceful shutdown.
type Shutdown interface {
	OnShutdown(ctx context.Context) error
}
