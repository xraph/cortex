package audithook

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/plugin"
)

// Compile-time interface checks.
var (
	_ plugin.Extension          = (*Extension)(nil)
	_ plugin.RunStarted         = (*Extension)(nil)
	_ plugin.RunCompleted       = (*Extension)(nil)
	_ plugin.RunFailed          = (*Extension)(nil)
	_ plugin.ToolCalled         = (*Extension)(nil)
	_ plugin.ToolCompleted      = (*Extension)(nil)
	_ plugin.ToolFailed         = (*Extension)(nil)
	_ plugin.PersonaResolved    = (*Extension)(nil)
	_ plugin.BehaviorTriggered  = (*Extension)(nil)
	_ plugin.CheckpointCreated  = (*Extension)(nil)
	_ plugin.CheckpointResolved = (*Extension)(nil)
)

// Recorder is the interface that audit backends must implement.
// Matches chronicle.Emitter but defined locally to avoid the import.
type Recorder interface {
	Record(ctx context.Context, event *AuditEvent) error
}

// AuditEvent mirrors chronicle/audit.Event without a module dependency.
type AuditEvent struct {
	Action     string         `json:"action"`
	Resource   string         `json:"resource"`
	Category   string         `json:"category"`
	ResourceID string         `json:"resource_id,omitempty"`
	Metadata   map[string]any `json:"metadata,omitempty"`
	Outcome    string         `json:"outcome"`
	Severity   string         `json:"severity"`
	Reason     string         `json:"reason,omitempty"`
}

// RecorderFunc is an adapter to use a plain function as a Recorder.
type RecorderFunc func(ctx context.Context, event *AuditEvent) error

func (f RecorderFunc) Record(ctx context.Context, event *AuditEvent) error {
	return f(ctx, event)
}

// Extension bridges Cortex lifecycle events to an audit trail backend.
type Extension struct {
	recorder Recorder
	enabled  map[string]bool
	logger   *slog.Logger
}

// New creates an Extension that emits audit events through the provided Recorder.
func New(r Recorder, opts ...Option) *Extension {
	e := &Extension{
		recorder: r,
		logger:   slog.Default(),
	}
	for _, opt := range opts {
		opt(e)
	}
	return e
}

// Name implements plugin.Extension.
func (e *Extension) Name() string { return "audit-hook" }

func (e *Extension) OnRunStarted(ctx context.Context, agentID id.AgentID, runID id.AgentRunID, input string) error {
	return e.record(ctx, ActionRunStarted, SeverityInfo, OutcomeSuccess,
		ResourceRun, runID.String(), CategoryAgent, nil,
		"agent_id", agentID.String(),
		"input_length", len(input),
	)
}

func (e *Extension) OnRunCompleted(ctx context.Context, agentID id.AgentID, runID id.AgentRunID, _ string, elapsed time.Duration) error {
	return e.record(ctx, ActionRunCompleted, SeverityInfo, OutcomeSuccess,
		ResourceRun, runID.String(), CategoryAgent, nil,
		"agent_id", agentID.String(),
		"elapsed_ms", elapsed.Milliseconds(),
	)
}

func (e *Extension) OnRunFailed(ctx context.Context, agentID id.AgentID, runID id.AgentRunID, runErr error) error {
	return e.record(ctx, ActionRunFailed, SeverityCritical, OutcomeFailure,
		ResourceRun, runID.String(), CategoryAgent, runErr,
		"agent_id", agentID.String(),
	)
}

func (e *Extension) OnToolCalled(ctx context.Context, runID id.AgentRunID, toolName string, _ any) error {
	return e.record(ctx, ActionToolCalled, SeverityInfo, OutcomeSuccess,
		ResourceTool, runID.String(), CategoryTool, nil,
		"tool_name", toolName,
	)
}

func (e *Extension) OnToolCompleted(ctx context.Context, runID id.AgentRunID, toolName, _ string, elapsed time.Duration) error {
	return e.record(ctx, ActionToolCompleted, SeverityInfo, OutcomeSuccess,
		ResourceTool, runID.String(), CategoryTool, nil,
		"tool_name", toolName,
		"elapsed_ms", elapsed.Milliseconds(),
	)
}

func (e *Extension) OnToolFailed(ctx context.Context, runID id.AgentRunID, toolName string, toolErr error) error {
	return e.record(ctx, ActionToolFailed, SeverityCritical, OutcomeFailure,
		ResourceTool, runID.String(), CategoryTool, toolErr,
		"tool_name", toolName,
	)
}

func (e *Extension) OnPersonaResolved(ctx context.Context, agentID id.AgentID, personaName string) error {
	return e.record(ctx, ActionPersonaResolved, SeverityInfo, OutcomeSuccess,
		ResourcePersona, agentID.String(), CategoryPersona, nil,
		"persona_name", personaName,
	)
}

func (e *Extension) OnBehaviorTriggered(ctx context.Context, runID id.AgentRunID, behaviorName string) error {
	return e.record(ctx, ActionBehaviorTriggered, SeverityInfo, OutcomeSuccess,
		ResourceBehavior, runID.String(), CategoryPersona, nil,
		"behavior_name", behaviorName,
	)
}

func (e *Extension) OnCheckpointCreated(ctx context.Context, cpID id.CheckpointID, runID id.AgentRunID, reason string) error {
	return e.record(ctx, ActionCheckpointCreated, SeverityInfo, OutcomeSuccess,
		ResourceCheckpoint, cpID.String(), CategoryCheckpoint, nil,
		"run_id", runID.String(),
		"reason", reason,
	)
}

func (e *Extension) OnCheckpointResolved(ctx context.Context, cpID id.CheckpointID, decision string) error {
	return e.record(ctx, ActionCheckpointResolved, SeverityInfo, OutcomeSuccess,
		ResourceCheckpoint, cpID.String(), CategoryCheckpoint, nil,
		"decision", decision,
	)
}

func (e *Extension) record(
	ctx context.Context,
	action, severity, outcome string,
	resource, resourceID, category string,
	err error,
	kvPairs ...any,
) error {
	if e.enabled != nil && !e.enabled[action] {
		return nil
	}

	meta := make(map[string]any, len(kvPairs)/2+1)
	for i := 0; i+1 < len(kvPairs); i += 2 {
		key, ok := kvPairs[i].(string)
		if !ok {
			key = fmt.Sprintf("%v", kvPairs[i])
		}
		meta[key] = kvPairs[i+1]
	}

	var reason string
	if err != nil {
		reason = err.Error()
		meta["error"] = err.Error()
	}

	evt := &AuditEvent{
		Action:     action,
		Resource:   resource,
		Category:   category,
		ResourceID: resourceID,
		Metadata:   meta,
		Outcome:    outcome,
		Severity:   severity,
		Reason:     reason,
	}

	if recErr := e.recorder.Record(ctx, evt); recErr != nil {
		e.logger.Warn("audit_hook: failed to record audit event",
			"action", action,
			"resource_id", resourceID,
			"error", recErr,
		)
	}
	return nil
}
