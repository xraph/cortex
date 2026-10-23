package optimization

import (
	"context"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// EnvironmentFactory owns durable isolation and remote cancellation.
type EnvironmentFactory interface {
	Open(context.Context, TrialRequest) (TrialEnvironment, error)
	Recover(context.Context, EnvironmentReceipt) (TrialEnvironment, error)
	Stop(context.Context, EnvironmentReceipt, Fence) (StopReceipt, error)
}

// TrialEnvironment reconnects to one attempt. Close releases the client handle;
// only EnvironmentFactory.Stop can establish remote termination or quarantine.
type TrialEnvironment interface {
	Receipt() EnvironmentReceipt
	Execute(context.Context, *engine.PreparedAgent, string, engine.PreparedRunOptions) (*ExecutionRecord, error)
	Inspect(context.Context, id.ID) (AttemptStatus, error)
	Close(context.Context) error
}

// Fence identifies the current generation allowed to control an environment.
type Fence struct {
	Owner      string `json:"owner"`
	Generation int64  `json:"generation"`
}

// IsolationCapabilities are host guarantees, not inferred from session IDs.
type IsolationCapabilities struct {
	Isolated              bool `json:"isolated"`
	Resettable            bool `json:"resettable"`
	IdempotentEffects     bool `json:"idempotent_effects"`
	Recoverable           bool `json:"recoverable"`
	Stoppable             bool `json:"stoppable"`
	ReadOnly              bool `json:"read_only"`
	FrozenRetrieval       bool `json:"frozen_retrieval"`
	AtomicSources         bool `json:"atomic_sources"`
	DeterministicApproval bool `json:"deterministic_approval"`
}

// EnvironmentReceipt must be persisted before execution begins.
type EnvironmentReceipt struct {
	ID            string                `json:"id"`
	Provider      string                `json:"provider"`
	EnvironmentID string                `json:"environment_id"`
	Scope         cortex.Scope          `json:"scope"`
	AttemptID     id.ID                 `json:"attempt_id"`
	Snapshot      ContentRef            `json:"snapshot"`
	Capabilities  IsolationCapabilities `json:"capabilities"`
	Fence         Fence                 `json:"fence"`
	CreatedAt     time.Time             `json:"created_at"`
}

// TrialRequest provisions one idempotent attempt from a frozen task snapshot.
type TrialRequest struct {
	// Allowance and Limits must be enforced by the host environment.
	Allowance    TrialAllowance `json:"allowance"`
	Limits       Limits         `json:"limits"`
	Scope        cortex.Scope   `json:"scope"`
	ExperimentID id.ID          `json:"experiment_id"`
	TrialID      id.ID          `json:"trial_id"`
	AttemptID    id.ID          `json:"attempt_id"`
	Task         TaskCase       `json:"task"`
	Version      VersionRef     `json:"version"`
	Fence        Fence          `json:"fence"`
	Deadline     time.Time      `json:"deadline"`
}

// StopState distinguishes positive containment evidence from an ambiguous reply.
type StopState string

const (
	StopTerminated  StopState = "terminated"
	StopQuarantined StopState = "quarantined"
	StopUnknown     StopState = "unknown"
)

// StopReceipt records authoritative containment for a fenced attempt.
type StopReceipt struct {
	EnvironmentReceiptID string     `json:"environment_receipt_id"`
	AttemptID            id.ID      `json:"attempt_id"`
	Fence                Fence      `json:"fence"`
	State                StopState  `json:"state"`
	Evidence             ContentRef `json:"evidence"`
	ObservedAt           time.Time  `json:"observed_at"`
}

// AttemptStatus recovers the original result without dispatching another call.
type AttemptStatus struct {
	AttemptID  id.ID            `json:"attempt_id"`
	State      TrialState       `json:"state"`
	Execution  *ExecutionRecord `json:"execution,omitempty"`
	ObservedAt time.Time        `json:"observed_at"`
}

// ExecutionRecord captures the original run and accounting for later evaluation.
type ExecutionRecord struct {
	RunID          id.AgentRunID      `json:"run_id"`
	StepIDs        []id.ID            `json:"step_ids"`
	ToolCallIDs    []id.ID            `json:"tool_call_ids"`
	PreparedDigest string             `json:"prepared_digest"`
	InputDigest    string             `json:"input_digest"`
	Outcome        run.Outcome        `json:"outcome"`
	State          run.State          `json:"state"`
	Usage          Usage              `json:"usage"`
	StartedAt      time.Time          `json:"started_at"`
	CompletedAt    time.Time          `json:"completed_at"`
	Duration       time.Duration      `json:"duration"`
	Environment    EnvironmentReceipt `json:"environment"`
	Artifacts      []ArtifactRef      `json:"artifacts"`
	Digest         string             `json:"digest"`
}
