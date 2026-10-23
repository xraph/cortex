package verification

import (
	"context"
	"math"
	"slices"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// PrefixCaptureIntent identifies immutable pre-generation capture policy.
const PrefixCaptureIntent id.Prefix = "vintent"

// CaptureIntent pins the job and result identities before generation. Template
// uses the run's registration revision and an empty-content candidate reference
// (zero bytes and Digest(nil)). Schema 2 also reserves a transcript outside the
// template. Capture substitutes the completed revision and content hashes/byte
// counts, then appends the transcript reference. All other policy stays immutable.
// Candidate.CreatedAt records when the host reserved that artifact identity.
// The host must reserve EvaluationBudget separately from generation allowance.
type CaptureIntent struct {
	Transcript         *evidence.Ref `json:"transcript,omitempty"`
	MaxTranscriptBytes int64         `json:"max_transcript_bytes,omitempty"`
	SchemaVersion      int           `json:"schema_version"`
	ID                 id.ID         `json:"id"`
	Template           JobRequest    `json:"template"`
	ResultID           id.ID         `json:"result_id"`
	MaxCandidateBytes  int64         `json:"max_candidate_bytes"`
	Digest             string        `json:"digest"`
}

// SealCaptureIntent validates and detaches a trusted host's capture template.
func SealCaptureIntent(intent CaptureIntent) (CaptureIntent, error) {
	// Leave room for finalization, enqueue and result commit to advance the run.
	if intent.Template.ExpectedRunRevision >= math.MaxInt64-2 {
		return CaptureIntent{}, ErrInvalid
	}
	if (intent.SchemaVersion != 1 && intent.SchemaVersion != 2) || intent.ID.IsNil() || intent.ID.Prefix() != PrefixCaptureIntent || intent.ResultID.IsNil() || intent.ResultID.Prefix() != PrefixResult || intent.MaxCandidateBytes <= 0 || intent.Template.EvaluationBudget == nil || intent.Template.Binding.Candidate.Bytes != 0 || intent.Template.Binding.Candidate.SHA256 != evidence.Digest(nil) {
		return CaptureIntent{}, ErrInvalid
	}
	if err := validateTranscriptReservation(intent); err != nil {
		return CaptureIntent{}, err
	}
	// Detach the reservation as well as the template.
	if intent.Transcript != nil {
		ref := *intent.Transcript
		ref.Scope.Levels = slices.Clone(ref.Scope.Levels)
		intent.Transcript = &ref
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
// Schema 2 requires the canonical transcript. Storage must validate run identity
// and accounting under its transaction. The intent remains unchanged.
func (i CaptureIntent) JobFor(revision int64, output string, transcript ...[]byte) (JobRequest, error) {
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
	if i.SchemaVersion == 1 {
		if len(transcript) != 0 {
			return JobRequest{}, ErrInvalid
		}
	} else {
		if len(transcript) != 1 {
			return JobRequest{}, ErrInvalid
		}
		if int64(len(transcript[0])) > i.MaxTranscriptBytes {
			return JobRequest{}, evidence.ErrTooLarge
		}
		trace, decodeErr := DecodeExecutionTranscript(transcript[0])
		if decodeErr != nil || trace.RunID != req.Binding.RunID || trace.InputsDigest != req.Binding.Context.ExecutionInputs.SHA256 || trace.Messages[len(trace.Messages)-1].Content != output {
			return JobRequest{}, ErrInvalid
		}
		ref := *i.Transcript
		ref.Scope.Levels = slices.Clone(ref.Scope.Levels)
		ref.SHA256, ref.Bytes = evidence.Digest(transcript[0]), int64(len(transcript[0]))
		req.Binding.Context.ExecutionTranscript = &ref
		req.Binding.Evidence = append(req.Binding.Evidence, ref)
	}
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

func validateTranscriptReservation(i CaptureIntent) error {
	if i.Template.Binding.Context.ExecutionTranscript != nil {
		return ErrInvalid
	}
	if i.SchemaVersion == 1 {
		if i.Transcript != nil || i.MaxTranscriptBytes != 0 {
			return ErrInvalid
		}
		return nil
	}
	ref := i.Transcript
	if ref == nil || ref.Validate() != nil || ref.Bytes != 0 || ref.SHA256 != evidence.Digest(nil) || i.MaxTranscriptBytes <= 0 || i.Template.Binding.Context.ExecutionInputs == nil || ref.Scope.Canonical() != i.Template.Binding.Scope.Canonical() || ref.ID == i.Template.Binding.Candidate.ID || len(i.Template.Binding.Evidence) >= 256 {
		return ErrInvalid
	}
	for _, existing := range i.Template.Binding.Evidence {
		if existing.ID == ref.ID {
			return ErrInvalid
		}
	}
	return nil
}
