package verification

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
)

// HumanVerdict records an authenticated host review of exact evidence.
// EvidenceDigest is the complete Binding.Digest, not merely candidate bytes.
type HumanVerdict struct {
	Actor            string    `json:"actor"`
	DecisionRevision string    `json:"decision_revision"`
	EvidenceDigest   string    `json:"evidence_digest"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// Outcome describes one check. Scores are optional diagnostics in [0,1].
// Status is the authoritative verdict; scores never turn failures into passes.
type Outcome struct {
	CriterionID      string        `json:"criterion_id"`
	Verifier         string        `json:"verifier"`
	VerifierRevision string        `json:"verifier_revision"`
	Status           Status        `json:"status"`
	Score            *float64      `json:"score,omitempty"`
	EvidenceIDs      []id.ID       `json:"evidence_ids,omitempty"`
	Human            *HumanVerdict `json:"human,omitempty"`
	Reason           string        `json:"reason,omitempty"`
}

// Report is an immutable, host-stamped observation. SealReport computes
// integrity, not authenticity. Only trusted verifier adapters may create reports.
type Report struct {
	SchemaVersion int       `json:"schema_version"`
	ID            id.ID     `json:"id"`
	Digest        string    `json:"digest"`
	Binding       Binding   `json:"binding"`
	CheckedAt     time.Time `json:"checked_at"`
	ExpiresAt     time.Time `json:"expires_at"`
	Outcomes      []Outcome `json:"outcomes"`
}

// SealReport validates and detaches a report before computing its digest.
func SealReport(r Report) (Report, error) {
	r.Digest = ""
	if err := r.validateFields(); err != nil {
		return Report{}, err
	}
	data, err := json.Marshal(r)
	if err != nil {
		return Report{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if len(data) > 1<<20 {
		return Report{}, ErrInvalid
	}
	var detached Report
	if err = json.Unmarshal(data, &detached); err != nil {
		return Report{}, fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	detached.Digest = evidence.Digest(data)
	return detached, nil
}

func (r Report) validateFields() error {
	if r.SchemaVersion != 1 || r.ID.IsNil() || r.ID.Prefix() != PrefixReport || r.CheckedAt.IsZero() || !r.ExpiresAt.After(r.CheckedAt) || len(r.Outcomes) == 0 || len(r.Outcomes) > 256 {
		return ErrInvalid
	}
	if err := r.Binding.Validate(); err != nil {
		return err
	}
	refs := append([]evidence.Ref{r.Binding.Candidate}, r.Binding.Evidence...)
	known := make(map[id.ID]bool, len(refs))
	for _, ref := range refs {
		if ref.CreatedAt.After(r.CheckedAt) {
			return ErrInvalid
		}
		known[ref.ID] = true
	}
	for _, source := range r.Binding.Context.Sources {
		if source.RetrievedAt.After(r.CheckedAt) {
			return ErrInvalid
		}
	}
	seen := make(map[string]bool, len(r.Outcomes))
	for _, outcome := range r.Outcomes {
		if !validText(outcome.CriterionID, outcome.Verifier, outcome.VerifierRevision, outcome.Reason) {
			return ErrInvalid
		}
		if strings.TrimSpace(outcome.CriterionID) == "" || seen[outcome.CriterionID] || strings.TrimSpace(outcome.Verifier) == "" || strings.TrimSpace(outcome.VerifierRevision) == "" {
			return ErrInvalid
		}
		seen[outcome.CriterionID] = true
		switch outcome.Status {
		case StatusPassed, StatusFailed, StatusInconclusive:
		default:
			return ErrInvalid
		}
		if outcome.Score != nil && (math.IsNaN(*outcome.Score) || math.IsInf(*outcome.Score, 0) || *outcome.Score < 0 || *outcome.Score > 1) {
			return ErrInvalid
		}
		evidenceSeen := make(map[id.ID]bool, len(outcome.EvidenceIDs))
		for _, refID := range outcome.EvidenceIDs {
			if !known[refID] || evidenceSeen[refID] {
				return ErrInvalid
			}
			evidenceSeen[refID] = true
		}
		if h := outcome.Human; h != nil {
			if !validText(h.Actor, h.DecisionRevision) {
				return ErrInvalid
			}
			digest, err := r.Binding.Digest()
			if err != nil {
				return err
			}
			if strings.TrimSpace(h.Actor) == "" || strings.TrimSpace(h.DecisionRevision) == "" || h.EvidenceDigest != digest || !h.ExpiresAt.After(r.CheckedAt) {
				return ErrInvalid
			}
		}
	}
	return nil
}

// Validate rejects tampering without updating the recorded digest.
func (r Report) Validate() error {
	if err := r.validateFields(); err != nil {
		return err
	}
	expected := r.Digest
	r.Digest = ""
	actual, err := recordDigest(r)
	if err != nil {
		return err
	}
	if expected != actual {
		return fmt.Errorf("%w: report digest", ErrInvalid)
	}
	return nil
}

// ValidateReportForContract rejects outcomes from unknown criteria or verifier
// revisions. Partial reports are allowed; Aggregate handles missing checks.
func ValidateReportForContract(c Contract, r Report) error {
	if err := c.Validate(); err != nil {
		return err
	}
	if err := r.Validate(); err != nil {
		return err
	}
	if r.Binding.ContractDigest != c.Digest || r.Binding.Scope.Canonical() != c.Scope.Canonical() {
		return ErrInvalid
	}
	criteria := make(map[string]Criterion, len(c.Criteria))
	for _, criterion := range c.Criteria {
		criteria[criterion.ID] = criterion
	}
	for _, outcome := range r.Outcomes {
		criterion, ok := criteria[outcome.CriterionID]
		if !ok || outcome.Verifier != criterion.Verifier || outcome.VerifierRevision != criterion.VerifierRevision || (outcome.Human != nil && criterion.Kind != KindHuman) {
			return ErrInvalid
		}
	}
	return nil
}
