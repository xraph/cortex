package learning

import (
	"slices"
	"strings"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

// ClassifyActivity labels an ordinary lifecycle observation. Completion alone
// never establishes a validated result, even if a caller mentions verification.
func ClassifyActivity(state run.State) EvidenceClass {
	if state == run.StateFailed {
		return EvidenceFailedApproach
	}
	return EvidenceObservation
}

// ValidatePurpose permits only completed, passed ordinary runs as lesson sources.
func ValidatePurpose(purpose run.ExecutionPurpose, state run.State, status verification.Status) error {
	if (purpose != "" && purpose != run.PurposeNormal) || state != run.StateCompleted || status != verification.StatusPassed {
		return ErrIneligible
	}
	return nil
}

// ValidateProvenance rejects recalled lessons as independent evidence, including
// legacy source markers. Callers must supply the authoritative saved run.
func ValidateProvenance(provenance run.ExecutionProvenance) error {
	if provenance.UnattributedHistory || len(provenance.RecalledLessons) != 0 {
		return ErrIneligible
	}
	for _, refs := range [][]run.Revision{provenance.Sources, provenance.SourceReads} {
		for _, ref := range refs {
			if strings.HasPrefix(ref.Name, "lesson:") {
				return ErrIneligible
			}
		}
	}
	return nil
}

// Validate checks stored ordinary lesson shape and expiry. It cannot authorize
// content or prove that the reports are still available; Resolve must do both.
func (lesson Lesson) Validate(at time.Time) error {
	if lesson.ID.IsNil() || lesson.ID.Prefix() != PrefixLesson || cortex.ValidateScope(lesson.Scope) != nil || lesson.HarnessSchema != 1 || lesson.EvidenceClass != EvidenceValidated || lesson.Revision < 1 || lesson.CreatedAt.IsZero() || at.Before(lesson.CreatedAt) || !lesson.ExpiresAt.After(at) || !lesson.RevokedAt.IsZero() || lesson.SupersededBy != nil || lesson.Result == nil || lesson.Result.Validate() != nil || len(lesson.DecisionIDs) != 0 || len(lesson.TrialIDs) != 0 || len(lesson.SourceReports) == 0 || len(lesson.SourceReports) > 256 {
		return ErrIneligible
	}
	if ValidateContent(lesson.Domain, lesson.Statement, lesson.Applicability, lesson.Contraindications) != nil {
		return ErrInvalid
	}
	seen := make(map[id.ID]bool)
	for _, ref := range lesson.SourceReports {
		if ref.ID.IsNil() || ref.ID.Prefix() != verification.PrefixReport || !evidence.ValidDigest(ref.Digest) || seen[ref.ID] {
			return ErrInvalid
		}
		seen[ref.ID] = true
	}
	return nil
}

// ValidateSnapshot reuses committed-result and report freshness validation.
// Ordinary lessons preserve the exact verified output; a derived claim needs
// its own verification. Source lessons cannot attest themselves through recall.
func ValidateSnapshot(lesson Lesson, snapshot verification.ResultSnapshot, at time.Time) error {
	if lesson.Validate(at) != nil || snapshot.Validate(*lesson.Result, at) != nil || lesson.Scope.Canonical() != snapshot.Receipt.Request.Current.Scope.Canonical() || lesson.Statement != snapshot.Output {
		return ErrIneligible
	}
	refs := make([]ReportRef, 0, len(snapshot.Reports))
	for _, report := range snapshot.Reports {
		refs = append(refs, ReportRef{ID: report.ID, Digest: report.Digest})
		if lesson.ExpiresAt.After(report.ExpiresAt) {
			return ErrIneligible
		}
	}
	if !slices.Equal(refs, lesson.SourceReports) {
		return ErrIneligible
	}
	sources := snapshot.Receipt.Request.Current.Context.Sources
	if verification.ValidateSources(sources) != nil {
		return ErrIneligible
	}
	for _, source := range sources {
		// A recalled lesson is context, never independent evidence for a new lesson.
		if strings.HasPrefix(source.Resource, "lesson:") || source.Resource == lesson.ID.String() {
			return ErrIneligible
		}
	}
	return nil
}
