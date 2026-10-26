package suspension

import (
	"context"
	"encoding/json"
	"math"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/event"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// ResumeClosureRequest closes an uncertain start as incomplete. The host must
// authenticate permanent worker and tool quiescence, including delayed actions,
// for this exact admission, start and current run revision. A deadline, a stopped
// local process or a digest alone does not establish that external work stopped.
// Closure never accepts output, refunds usage, clears effects or permits replay.
type ResumeClosureRequest struct {
	SchemaVersion   int          `json:"schema_version"`
	Scope           cortex.Scope `json:"scope"`
	Version         ClaimVersion `json:"version"`
	AdmissionDigest string       `json:"admission_digest"`
	StartDigest     string       `json:"start_digest"`
	RunRevision     int64        `json:"run_revision"`
	Actor           string       `json:"actor"`
	Issuer          string       `json:"issuer"`
	IssuerRevision  string       `json:"issuer_revision"`
	ProofRef        string       `json:"proof_ref"`
	ProofDigest     string       `json:"proof_digest"`
	ObservedAt      time.Time    `json:"observed_at"`
	ExpiresAt       time.Time    `json:"expires_at"`
}

// Freeze checks structure and detaches the request. It does not authenticate proof.
func (r ResumeClosureRequest) Freeze() (ResumeClosureRequest, string, error) {
	if r.SchemaVersion != 1 || cortex.ValidateScope(r.Scope) != nil || r.Version.RunID.Prefix() != id.PrefixAgentRun || r.Version.SuspensionID.Prefix() != id.PrefixSuspension || r.Version.RunRevision < 0 || r.RunRevision <= r.Version.RunRevision || r.RunRevision == math.MaxInt64 || !evidence.ValidDigest(r.AdmissionDigest) || !evidence.ValidDigest(r.StartDigest) || !evidence.ValidDigest(r.ProofDigest) || !closureText(r.Actor, 512) || !closureText(r.Issuer, 512) || !closureText(r.IssuerRevision, 512) || !closureText(r.ProofRef, 4096) || r.ObservedAt.IsZero() || !r.ExpiresAt.After(r.ObservedAt) || r.ExpiresAt.Sub(r.ObservedAt) > 5*time.Minute {
		return ResumeClosureRequest{}, "", ErrResumeAdmissionConflict
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > 16<<10 {
		return ResumeClosureRequest{}, "", ErrResumeAdmissionConflict
	}
	var frozen ResumeClosureRequest
	if json.Unmarshal(data, &frozen) != nil {
		return ResumeClosureRequest{}, "", ErrResumeAdmissionConflict
	}
	return frozen, evidence.Digest(data), nil
}

func closureText(s string, limit int) bool {
	return len(s) <= limit && strings.TrimSpace(s) != "" && utf8.ValidString(s) && !strings.ContainsRune(s, '\x00')
}

// Digest binds a retained start without its transient execution authority.
func (s ResumeStart) Digest() (string, error) {
	if s.SchemaVersion != 1 || cortex.ValidateScope(s.Scope) != nil || s.Version.RunID.Prefix() != id.PrefixAgentRun || s.Version.SuspensionID.Prefix() != id.PrefixSuspension || s.Version.RunRevision < 0 || !evidence.ValidDigest(s.RequestDigest) || s.StartedAt.IsZero() {
		return "", ErrResumeAdmissionConflict
	}
	data, err := json.Marshal(s)
	if err != nil {
		return "", ErrResumeAdmissionConflict
	}
	return evidence.Digest(data), nil
}

// ResumeClosure is immutable evidence of an incomplete terminal transition.
// Its metadata event is committed in the same transaction as the run state.
type ResumeClosure struct {
	Request       ResumeClosureRequest `json:"request"`
	RequestDigest string               `json:"request_digest"`
	EventID       id.ID                `json:"event_id"`
	RunRevision   int64                `json:"run_revision"`
	ClosedAt      time.Time            `json:"closed_at"`
	Fresh         bool                 `json:"-"`
}

// Validate checks receipt integrity without granting access or new execution.
func (r ResumeClosure) Validate() error {
	_, digest, err := r.Request.Freeze()
	if err != nil || r.RequestDigest != digest || r.EventID.Prefix() != event.PrefixEvent || r.RunRevision != r.Request.RunRevision+1 || r.ClosedAt.Before(r.Request.ObservedAt) || !r.ClosedAt.Before(r.Request.ExpiresAt) {
		return ErrResumeAdmissionConflict
	}
	return nil
}

// ResumeClosureStore requires current host proof and authority before writes.
// Closure serializes with run writers, binds the latest admission and start,
// and rejects changed requests. Retry the identical request after a lost ack.
// Historical retries return the original receipt even after proof expiry.
type ResumeClosureStore interface {
	CloseResume(context.Context, ResumeClosureRequest) (ResumeClosure, error)
	GetResumeClosure(context.Context, id.AgentRunID, id.SuspensionID) (ResumeClosure, error)
}
