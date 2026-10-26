// Package learning defines lessons accepted from governed evidence.
package learning

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/verification"
)

// PrefixLesson identifies lessons accepted from ordinary verified runs.
const PrefixLesson id.Prefix = "lesson"

// EvidenceClass separates observations from claims supported by verification.
type EvidenceClass string

const (
	EvidenceValidated      EvidenceClass = "validated"
	EvidenceContrastive    EvidenceClass = "contrastive"
	EvidenceExploratory    EvidenceClass = "exploratory"
	EvidenceObservation    EvidenceClass = "observation"
	EvidenceFailedApproach EvidenceClass = "failed_approach"
)

// Stable errors distinguish unavailable authority, stale evidence and conflicts.
var (
	ErrInvalid     = errors.New("cortex: learning: invalid lesson")
	ErrDenied      = errors.New("cortex: learning: access denied")
	ErrConflict    = errors.New("cortex: learning: conflicting lesson revision")
	ErrUnavailable = errors.New("cortex: learning: capability unavailable")
	ErrNotFound    = errors.New("cortex: learning: lesson not found")
	ErrIneligible  = errors.New("cortex: learning: evidence is not eligible")
)

// ReportRef binds a source report to its exact immutable digest.
type ReportRef struct {
	ID     id.ID  `json:"id"`
	Digest string `json:"digest"`
}

// Lesson retains local eligibility even if remote publication or revocation fails.
type Lesson struct {
	ID                id.ID                           `json:"id"`
	Scope             cortex.Scope                    `json:"scope"`
	Domain            string                          `json:"domain"`
	HarnessSchema     int                             `json:"harness_schema"`
	Statement         string                          `json:"statement"`
	Applicability     []string                        `json:"applicability"`
	Contraindications []string                        `json:"contraindications"`
	DecisionIDs       []id.ID                         `json:"decision_ids"`
	TrialIDs          []id.ID                         `json:"trial_ids"`
	EvidenceClass     EvidenceClass                   `json:"evidence_class"`
	Revision          int64                           `json:"revision"`
	CreatedAt         time.Time                       `json:"created_at"`
	ExpiresAt         time.Time                       `json:"expires_at"`
	RevokedAt         time.Time                       `json:"revoked_at"`
	RevocationReason  string                          `json:"revocation_reason,omitempty"`
	Result            *verification.ResultReadRequest `json:"result,omitempty"`
	SourceReports     []ReportRef                     `json:"source_reports,omitempty"`
	Corrects          *Ref                            `json:"corrects,omitempty"`
	SupersededBy      *Ref                            `json:"superseded_by,omitempty"`
}

// Ref freezes local eligibility and content at experiment creation.
type Ref struct {
	ID       id.ID  `json:"id"`
	Revision int64  `json:"revision"`
	Digest   string `json:"digest"`
}

// Digest preserves the complete lesson's deterministic JSON representation.
func Digest(lesson Lesson) (string, error) {
	body, err := json.Marshal(lesson)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// ValidateContent bounds lesson text without asserting that its claim is true.
func ValidateContent(domain, statement string, applicability, contraindications []string) error {
	if strings.TrimSpace(domain) == "" || len(domain) > 256 || !utf8.ValidString(domain) || strings.TrimSpace(statement) == "" || len(statement) > 32768 || !utf8.ValidString(statement) {
		return ErrInvalid
	}
	for _, conditions := range [][]string{applicability, contraindications} {
		if len(conditions) > 64 {
			return ErrInvalid
		}
		for _, value := range conditions {
			if strings.TrimSpace(value) == "" || len(value) > 4096 || !utf8.ValidString(value) {
				return ErrInvalid
			}
		}
	}
	return nil
}
