package optimization

import (
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/learning"
)

// LessonDigest identifies the complete immutable publication payload.
func LessonDigest(lesson Lesson) (string, error) { return learning.Digest(lesson) }

// ValidateLessonDraft checks bounded reflection content before storage resolves evidence.
func ValidateLessonDraft(lesson Lesson) error {
	if _, err := ParseID(lesson.ID.String(), PrefixLesson); err != nil {
		return err
	}
	if _, err := ScopeKey(lesson.Scope); err != nil {
		return err
	}
	if lesson.Result != nil || len(lesson.SourceReports) != 0 || lesson.Corrects != nil || lesson.SupersededBy != nil {
		return ErrInvalidSpec
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
	if learning.ValidateContent(domain, statement, applicability, contraindications) != nil {
		return ErrInvalidSpec
	}
	return nil
}
