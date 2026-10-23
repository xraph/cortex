package coordination

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/planning"
	"github.com/xraph/cortex/verification"
)

const (
	PrefixReview   id.Prefix = "areview"
	MaxReviewBytes           = MaxHandoffBytes + 128<<10
)

var (
	ErrInvalidReview  = cortex.ErrInvalidReview
	ErrReviewConflict = cortex.ErrReviewConflict
	ErrReviewNotFound = cortex.ErrReviewNotFound
)

// ReviewAssessment is an advisory finding, never a verifier verdict.
type ReviewAssessment string

const (
	ReviewSupported    ReviewAssessment = "supported"
	ReviewContradicted ReviewAssessment = "contradicted"
	ReviewMissing      ReviewAssessment = "missing_evidence"
)

// ReviewFinding retains a criterion's rationale and exact input references.
// Supporting and contradictory findings require evidence. Missing evidence may
// name available references but cannot invent a reference for absent bytes.
type ReviewFinding struct {
	CriterionID string           `json:"criterion_id"`
	Assessment  ReviewAssessment `json:"assessment"`
	Reason      string           `json:"reason"`
	EvidenceIDs []id.ID          `json:"evidence_ids,omitempty"`
}

// Review binds advisory findings to a submitted assignment and a separate run.
// The host authenticates identities, resolves current grants and prepares the
// reviewer independently. Sealing provides integrity, not that authentication.
type Review struct {
	SchemaVersion       int             `json:"schema_version"`
	ID                  id.ID           `json:"id"`
	Handoff             Handoff         `json:"handoff"`
	SubjectAssignmentID id.ID           `json:"subject_assignment_id"`
	SubjectRevision     int64           `json:"subject_revision"`
	SubjectDigest       string          `json:"subject_digest"`
	BindingDigest       string          `json:"binding_digest"`
	RunID               id.AgentRunID   `json:"run_id"`
	RunRevision         int64           `json:"run_revision"`
	PreparedDigest      string          `json:"prepared_digest"`
	ReviewedAt          time.Time       `json:"reviewed_at"`
	Findings            []ReviewFinding `json:"findings"`
	Digest              string          `json:"digest"`
}

// ReviewOutput is the bounded structured document produced by the reviewer.
// Subject and handoff digests prevent reusing prose for another assignment.
type ReviewOutput struct {
	SchemaVersion int             `json:"schema_version"`
	HandoffDigest string          `json:"handoff_digest"`
	SubjectDigest string          `json:"subject_digest"`
	BindingDigest string          `json:"binding_digest"`
	Findings      []ReviewFinding `json:"findings"`
}

// Output returns the canonical reviewer document for these findings.
func (r Review) Output() ([]byte, error) {
	if r.validateFields() != nil {
		return nil, ErrInvalidReview
	}
	return json.Marshal(ReviewOutput{1, r.Handoff.Digest, r.SubjectDigest, r.BindingDigest, r.Findings})
}

// ValidateOutput accepts equivalent JSON formatting while refusing unknown
// fields, trailing documents and any changed subject, evidence or finding.
func (r Review) ValidateOutput(data []byte) error {
	if len(data) == 0 || len(data) > MaxReviewBytes || r.Validate() != nil {
		return ErrInvalidReview
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var document ReviewOutput
	if decoder.Decode(&document) != nil {
		return ErrInvalidReview
	}
	var extra any
	if !errors.Is(decoder.Decode(&extra), io.EOF) {
		return ErrInvalidReview
	}
	expected := ReviewOutput{1, r.Handoff.Digest, r.SubjectDigest, r.BindingDigest, r.Findings}
	if !reflect.DeepEqual(document, expected) {
		return ErrInvalidReview
	}
	return nil
}

// SealReview detaches findings and seals the complete observation.
func SealReview(r Review) (Review, error) {
	r.Digest = ""
	if r.validateFields() != nil {
		return Review{}, ErrInvalidReview
	}
	data, err := json.Marshal(r)
	if err != nil || len(data) > MaxReviewBytes-80 {
		return Review{}, ErrInvalidReview
	}
	var frozen Review
	if json.Unmarshal(data, &frozen) != nil {
		return Review{}, ErrInvalidReview
	}
	frozen.Digest = evidence.Digest(data)
	return frozen, nil
}

// Validate checks integrity without granting authority or task acceptance.
func (r Review) Validate() error {
	sealed, err := SealReview(r)
	if err != nil || sealed.Digest != r.Digest {
		return ErrInvalidReview
	}
	return nil
}

func (r Review) validateFields() error {
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != PrefixReview || r.Handoff.Validate() != nil || r.Handoff.Purpose != HandoffReview || r.SubjectAssignmentID.IsNil() || r.SubjectAssignmentID.Prefix() != PrefixAssignment || r.SubjectAssignmentID == r.Handoff.AssignmentID || r.SubjectRevision < 1 || !evidence.ValidDigest(r.SubjectDigest) || !evidence.ValidDigest(r.BindingDigest) || r.RunID.IsNil() || r.RunID.Prefix() != id.PrefixAgentRun || r.RunID == r.Handoff.ParentRunID || r.RunRevision < 1 || !evidence.ValidDigest(r.PreparedDigest) || r.ReviewedAt.IsZero() || !r.ReviewedAt.Before(r.Handoff.Deadline) || len(r.Findings) != len(r.Handoff.Criteria) {
		return ErrInvalidReview
	}
	criteria := make(map[string]bool, len(r.Handoff.Criteria))
	for _, criterion := range r.Handoff.Criteria {
		criteria[criterion] = true
	}
	inputs := make(map[id.ID]bool, len(r.Handoff.Inputs))
	for _, ref := range r.Handoff.Inputs {
		inputs[ref.ID] = true
	}
	for _, finding := range r.Findings {
		if !criteria[finding.CriterionID] || !handoffText(finding.Reason, 4096) || len(finding.EvidenceIDs) > 64 {
			return ErrInvalidReview
		}
		delete(criteria, finding.CriterionID)
		switch finding.Assessment {
		case ReviewSupported, ReviewContradicted:
			if len(finding.EvidenceIDs) == 0 {
				return ErrInvalidReview
			}
		case ReviewMissing:
		default:
			return ErrInvalidReview
		}
		seen := make(map[id.ID]bool, len(finding.EvidenceIDs))
		for _, ref := range finding.EvidenceIDs {
			if !inputs[ref] || seen[ref] {
				return ErrInvalidReview
			}
			seen[ref] = true
		}
	}
	return nil
}

// ValidateReview checks current submission, plan, independent identity/context
// and all subject inputs. The caller must authenticate these records and load
// retained bytes. Findings never enter verification.Aggregate as verdicts.
func ValidateReview(r Review, subject Assignment, binding verification.Binding, p planning.Plan, at time.Time) error {
	if r.Validate() != nil || subject.Validate() != nil || subject.State != AssignmentSubmitted || subject.Submission == nil || ValidateHandoffPlan(r.Handoff, p, at) != nil || ValidateHandoffPlan(subject.Handoff, p, at) != nil || r.ReviewedAt.After(at) || r.ReviewedAt.Before(subject.At) {
		return ErrInvalidReview
	}
	digest, err := binding.Digest()
	if err != nil || digest != r.BindingDigest || digest != subject.Submission.BindingDigest || binding.Context.PrincipalRef != subject.Handoff.Receiver.Principal || binding.RunID != subject.Submission.RunID || r.SubjectAssignmentID != subject.Handoff.AssignmentID || r.SubjectRevision != subject.Revision || r.SubjectDigest != subject.Digest || r.Handoff.TaskID != subject.Handoff.TaskID || r.Handoff.ParentRunID != subject.Handoff.ParentRunID || r.Handoff.Sender != subject.Handoff.Sender || r.RunID == binding.RunID || r.PreparedDigest == binding.Context.PreparedDigest {
		return ErrInvalidReview
	}
	producer, reviewer := subject.Handoff.Receiver, r.Handoff.Receiver
	if producer.Principal == reviewer.Principal || (producer.Agent == reviewer.Agent && producer.Node == reviewer.Node) {
		return ErrInvalidReview
	}
	refs := append([]evidence.Ref{binding.Candidate}, binding.Evidence...)
	for _, ref := range refs {
		found := false
		for _, input := range r.Handoff.Inputs {
			if reflect.DeepEqual(ref, input) {
				found = true
				break
			}
		}
		if !found {
			return ErrInvalidReview
		}
	}
	for _, ref := range r.Handoff.Inputs {
		if ref.CreatedAt.After(r.ReviewedAt) || !ref.RetainUntil.After(at) {
			return ErrInvalidReview
		}
	}
	return nil
}

// ReviewStore retains independent observations without changing assignment or
// verifier status. Hosts authorize every call; historical reads confer no access
// to artifact bytes. Identical retries acknowledge the retained observation.
type ReviewStore interface {
	RecordAssignmentReview(context.Context, Review) (Review, error)
	GetAssignmentReview(context.Context, id.ID) (Review, error)
	ListAssignmentReviews(context.Context, ReviewListRequest) ([]Review, error)
}

// ReviewListRequest bounds exact-scope discovery for one producer assignment.
type ReviewListRequest struct {
	AssignmentID id.ID
	After        id.ID
	Limit        int
}
