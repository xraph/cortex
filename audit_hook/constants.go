// Package audithook bridges Cortex lifecycle events to an audit trail backend.
package audithook

// Severity constants.
const (
	SeverityInfo     = "info"
	SeverityWarning  = "warning"
	SeverityCritical = "critical"
)

// Outcome constants.
const (
	OutcomeSuccess = "success"
	OutcomeFailure = "failure"
)

// Action constants.
const (
	ActionRunStarted             = "cortex.agent.run.started"
	ActionRunCompleted           = "cortex.agent.run.completed"
	ActionRunFailed              = "cortex.agent.run.failed"
	ActionStepStarted            = "cortex.step.started"
	ActionStepCompleted          = "cortex.step.completed"
	ActionToolCalled             = "cortex.tool.called"
	ActionToolCompleted          = "cortex.tool.completed"
	ActionToolFailed             = "cortex.tool.failed"
	ActionPersonaResolved        = "cortex.persona.resolved"
	ActionSkillActivated         = "cortex.skill.activated"
	ActionBehaviorTriggered      = "cortex.behavior.triggered"
	ActionCognitivePhaseChanged  = "cortex.cognitive.phase_changed"
	ActionTraitApplied           = "cortex.trait.applied"
	ActionCheckpointCreated      = "cortex.checkpoint.created"
	ActionCheckpointResolved     = "cortex.checkpoint.resolved"
	ActionOrchestrationStarted   = "cortex.orchestration.started"
	ActionOrchestrationCompleted = "cortex.orchestration.completed"
	ActionAgentHandoff           = "cortex.agent.handoff"
)

// Resource constants.
const (
	ResourceAgent         = "agent"
	ResourceRun           = "run"
	ResourceTool          = "tool"
	ResourcePersona       = "persona"
	ResourceSkill         = "skill"
	ResourceBehavior      = "behavior"
	ResourceCheckpoint    = "checkpoint"
	ResourceOrchestration = "orchestration"
)

// Category constants.
const (
	CategoryAgent         = "agent"
	CategoryTool          = "tool"
	CategoryPersona       = "persona"
	CategoryCheckpoint    = "checkpoint"
	CategoryOrchestration = "orchestration"
)
