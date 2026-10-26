package learning

import (
	"testing"

	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func TestUncheckedCompletionIsObservation(t *testing.T) {
	if ClassifyActivity(run.StateCompleted) != EvidenceObservation {
		t.Fatal("unchecked completion became validated")
	}
}
func TestFailedRunCannotBecomeSuccessLesson(t *testing.T) {
	if ClassifyActivity(run.StateFailed) != EvidenceFailedApproach {
		t.Fatal("failed activity lost evidence class")
	}
	if ValidatePurpose(run.PurposeNormal, run.StateFailed, verification.StatusPassed) == nil {
		t.Fatal("failed run accepted as success evidence")
	}
}
func TestEvaluationRunsDoNotPolluteOrdinaryMemory(t *testing.T) {
	for _, purpose := range []run.ExecutionPurpose{run.PurposeEvaluation, run.PurposeOptimizationTrial, run.PurposeOptimizationController, "unknown"} {
		if ValidatePurpose(purpose, run.StateCompleted, verification.StatusPassed) == nil {
			t.Fatal("nonordinary purpose accepted", purpose)
		}
	}
	if err := ValidatePurpose(run.PurposeNormal, run.StateCompleted, verification.StatusPassed); err != nil {
		t.Fatal(err)
	}
}

func TestRecallProvenanceCannotAttestLessons(t *testing.T) {
	for _, provenance := range []run.ExecutionProvenance{
		{UnattributedHistory: true},
		{RecalledLessons: []run.Revision{{}}},
		{Sources: []run.Revision{{Name: "lesson:legacy"}}},
		{SourceReads: []run.Revision{{Name: "lesson:legacy"}}},
	} {
		if ValidateProvenance(provenance) == nil {
			t.Fatal("recalled context accepted as independent evidence")
		}
	}
	if err := ValidateProvenance(run.ExecutionProvenance{Sources: []run.Revision{{Name: "source:runbook"}}}); err != nil {
		t.Fatal(err)
	}
}
