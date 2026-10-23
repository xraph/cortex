package verification

import (
	"context"
	"math"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// PrefixCaptureIntent identifies immutable pre-generation capture policy.
const PrefixCaptureIntent id.Prefix = "vintent"

// CaptureIntent pins the job and result identities before generation. Template
// uses the run's registration revision and an empty-content candidate reference
// (zero bytes and Digest(nil)). Capture substitutes only the completed run
// revision, candidate hash and byte count. All other policy stays immutable.
// Candidate.CreatedAt records when the host reserved that artifact identity.
// The host must reserve EvaluationBudget separately from generation allowance.
type CaptureIntent struct {
	SchemaVersion     int        `json:"schema_version"`
	ID                id.ID      `json:"id"`
	Template          JobRequest `json:"template"`
	ResultID          id.ID      `json:"result_id"`
	MaxCandidateBytes int64      `json:"max_candidate_bytes"`
	Digest            string     `json:"digest"`
}

// SealCaptureIntent validates and detaches a trusted host's capture template.
func SealCaptureIntent(intent CaptureIntent) (CaptureIntent, error) {
	// Leave room for finalization, enqueue and result commit to advance the run.
	if intent.Template.ExpectedRunRevision >= math.MaxInt64-2 {
		return CaptureIntent{}, ErrInvalid
	}
	if intent.SchemaVersion != 1 || intent.ID.IsNil() || intent.ID.Prefix() != PrefixCaptureIntent || intent.ResultID.IsNil() || intent.ResultID.Prefix() != PrefixResult || intent.MaxCandidateBytes <= 0 || intent.Template.EvaluationBudget == nil || intent.Template.Binding.Candidate.Bytes != 0 || intent.Template.Binding.Candidate.SHA256 != evidence.Digest(nil) {
		return CaptureIntent{}, ErrInvalid
	}
	template, _, err := intent.Template.Freeze()
	if err != nil {
		return CaptureIntent{}, err
	}
	intent.Template = template
	intent.Digest = ""
	intent.Digest, err = recordDigest(intent)
	if err != nil {
		return CaptureIntent{}, err
	}
	return intent, nil
}

// Validate rejects altered or unsupported policy without resealing it.
func (i CaptureIntent) Validate() error {
	sealed, err := SealCaptureIntent(i)
	if err != nil {
		return err
	}
	if sealed.Digest != i.Digest {
		return ErrInvalid
	}
	return nil
}

// JobFor derives the exact request from a later saved run revision and output.
// Storage must obtain both values under its run transaction; model claims or
// caller-provided copies are not authoritative. The intent remains unchanged.
func (i CaptureIntent) JobFor(revision int64, output string) (JobRequest, error) {
	if err := i.Validate(); err != nil {
		return JobRequest{}, err
	}
	if revision <= i.Template.ExpectedRunRevision {
		return JobRequest{}, ErrInvalid
	}
	if int64(len(output)) > i.MaxCandidateBytes {
		return JobRequest{}, evidence.ErrTooLarge
	}
	req, _, err := i.Template.Freeze()
	if err != nil {
		return JobRequest{}, err
	}
	req.ExpectedRunRevision = revision
	req.Binding.Candidate.SHA256 = evidence.Digest([]byte(output))
	req.Binding.Candidate.Bytes = int64(len(output))
	frozen, _, err := req.Freeze()
	return frozen, err
}

// CaptureIntentStore persists a host's intent before dispatch and recovers it
// after the run finishes. Each exact-scope run, job and result identity has one
// immutable intent. Reads are historical metadata and need host authorization.
// Registering an intent alone does not install an execution or publication gate.
type CaptureIntentStore interface {
	CreateCaptureIntent(context.Context, CaptureIntent) error
	GetCaptureIntent(context.Context, id.ID) (CaptureIntent, error)
	GetRunCaptureIntent(context.Context, id.AgentRunID) (CaptureIntent, error)
	CaptureFromIntent(context.Context, id.ID) (Job, error)
}
