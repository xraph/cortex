package sandbox

import (
	"context"
	"fmt"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// ProfileDenyAll requires read-only root and inputs, empty capabilities, isolated
// PID/mount/network namespaces, non-root identity and enforced resource limits.
const ProfileDenyAll = "runsc-deny-all-v1"

// Capabilities is a trusted deployment attestation. The adapter must only publish
// Qualification after the pinned worker passes its live isolation probes.
// These values cannot establish their own authenticity.
type Capabilities struct {
	SchemaVersion  int       `json:"schema_version"`
	Runtime        string    `json:"runtime"`
	RuntimeDigest  string    `json:"runtime_digest"`
	Profile        string    `json:"profile"`
	Qualification  string    `json:"qualification"`
	QualifiedUntil time.Time `json:"qualified_until"`
	Limits         Limits    `json:"limits"`
}

// Validate checks capability shape and qualification expiry. It does not prove
// the attestation's origin or establish that live isolation probes passed.
func (c Capabilities) Validate() error {
	if c.SchemaVersion != 1 || c.Runtime != "runsc" || !evidence.ValidDigest(c.RuntimeDigest) || c.Profile != ProfileDenyAll || !text(c.Qualification, 512) || !time.Now().Before(c.QualifiedUntil) || !c.Limits.valid() {
		return fmt.Errorf("cortex: sandbox controls unavailable or unqualified")
	}
	return nil
}

// Supports fails closed for missing or expired qualification and excess limits.
func (c Capabilities) Supports(j Job) error {
	if err := j.Validate(); err != nil {
		return err
	}
	if err := c.Validate(); err != nil {
		return err
	}
	if !j.Limits.Within(c.Limits) {
		return fmt.Errorf("cortex: sandbox controls unavailable or unqualified")
	}
	return nil
}

// Runner submits and reconciles jobs by immutable identity. An ambiguous Submit
// must be inspected or retried with the same key, never replaced with a host run.
type Runner interface {
	Capabilities(context.Context) (Capabilities, error)
	Submit(context.Context, Job) (Receipt, error)
	Inspect(context.Context, id.ID) (JobStatus, error)
	Cancel(context.Context, id.ID) error
	Collect(context.Context, id.ID) (Result, error)
}

// Receipt binds accepted execution controls to an authenticated broker identity.
// Transport authentication is mandatory; Signature signs canonical JSON with
// this field empty using the broker's Ed25519 key.
type Receipt struct {
	SchemaVersion int          `json:"schema_version"`
	Job           Job          `json:"job"`
	Capabilities  Capabilities `json:"capabilities"`
	Broker        string       `json:"broker"`
	AcceptedAt    time.Time    `json:"accepted_at"`
	Signature     []byte       `json:"signature"`
}

// State describes execution only. Exited does not imply verified correctness.
type State string

const (
	StateAccepted  State = "accepted"
	StateRunning   State = "running"
	StateExited    State = "exited"
	StateCancelled State = "cancelled"
	StateFailed    State = "failed"
)

// JobStatus is a durable broker observation. Revision is a monotonic CAS token.
type JobStatus struct {
	Receipt   Receipt   `json:"receipt"`
	State     State     `json:"state"`
	Revision  uint64    `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Result is bounded output from an authenticated runner. The candidate's stdout
// never supplies a trusted verdict; protected grading evaluates it separately.
type Result struct {
	JobDigest string         `json:"job_digest"`
	State     State          `json:"state"`
	ExitCode  int            `json:"exit_code"`
	Reason    string         `json:"reason"`
	Stdout    []byte         `json:"stdout,omitempty"`
	Stderr    []byte         `json:"stderr,omitempty"`
	Artifacts []evidence.Ref `json:"artifacts,omitempty"`
	Signature []byte         `json:"signature,omitempty"`
}
