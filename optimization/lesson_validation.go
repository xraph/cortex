package optimization

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// LessonDigest identifies the complete immutable publication payload.
func LessonDigest(lesson Lesson) (string, error) {
	encoded, err := canonical.Marshal(lesson)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// ValidateLessonDraft checks bounded reflection content before storage resolves evidence.
func ValidateLessonDraft(lesson Lesson) error {
	if _, err := ParseID(lesson.ID.String(), PrefixLesson); err != nil {
		return err
	}
	if _, err := ScopeKey(lesson.Scope); err != nil {
		return err
	}
	if lesson.HarnessSchema != 1 || lesson.Revision != 0 || !lesson.CreatedAt.IsZero() || !lesson.RevokedAt.IsZero() || lesson.RevocationReason != "" {
		return ErrInvalidSpec
	}
	if err := validateLessonContent(lesson.Domain, lesson.Statement, lesson.Applicability, lesson.Contraindications); err != nil {
		return err
	}
	if len(lesson.DecisionIDs) == 0 || len(lesson.DecisionIDs) > 64 || len(lesson.TrialIDs) > 500 {
		return ErrIncompleteEvidence
	}
	for _, refs := range []struct {
		ids    []id.ID
		prefix id.Prefix
	}{{lesson.DecisionIDs, PrefixDecision}, {lesson.TrialIDs, PrefixTrial}} {
		seen := make(map[string]bool)
		for _, ref := range refs.ids {
			if _, err := ParseID(ref.String(), refs.prefix); err != nil {
				return err
			}
			if seen[ref.String()] {
				return ErrIncompleteEvidence
			}
			seen[ref.String()] = true
		}
	}
	switch lesson.EvidenceClass {
	case EvidenceValidated, EvidenceContrastive, EvidenceExploratory:
		return nil
	default:
		return ErrInvalidSpec
	}
}

func validateLessonContent(domain, statement string, applicability, contraindications []string) error {
	if strings.TrimSpace(domain) == "" || len(domain) > 256 || !utf8.ValidString(domain) || strings.TrimSpace(statement) == "" || len(statement) > 32768 || !utf8.ValidString(statement) {
		return ErrInvalidSpec
	}
	for _, conditions := range [][]string{applicability, contraindications} {
		if len(conditions) > 64 {
			return ErrInvalidSpec
		}
		for _, value := range conditions {
			if strings.TrimSpace(value) == "" || len(value) > 4096 || !utf8.ValidString(value) {
				return ErrInvalidSpec
			}
		}
	}
	return nil
}
