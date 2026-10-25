package optimization

import (
	"testing"

	"github.com/xraph/sentinel/evalplan"

	opt "github.com/xraph/cortex/optimization"
)

func TestTaskCriteriaExposeExpectedFallback(t *testing.T) {
	for _, test := range []struct {
		criterion evalplan.Criterion
		required  bool
	}{
		{evalplan.Criterion{Scorer: "exact"}, true},
		{evalplan.Criterion{Scorer: "contains"}, true},
		{evalplan.Criterion{Scorer: "not_contains", Config: map[string]any{"substring": ""}}, true},
		{evalplan.Criterion{Scorer: "contains", Config: map[string]any{"substring": "section heading"}}, false},
		{evalplan.Criterion{Scorer: "json_valid"}, false},
		{evalplan.Criterion{Scorer: "quality", RequiresExpected: true}, true},
	} {
		if got := criterionRequiresExpected(test.criterion); got != test.required {
			t.Fatalf("%+v: expected-answer requirement=%v", test.criterion, got)
		}
	}
}

func TestTaskCriteriaPreserveLegacyPlanWithoutTrustingForgedCapability(t *testing.T) {
	required, optional := true, false
	current := opt.EvaluationPlan{Criteria: []opt.CriterionDefinition{{ID: "quality", RequiresExpected: &required}}}
	legacy := opt.EvaluationPlan{Criteria: []opt.CriterionDefinition{{ID: "quality"}}}
	if !compatiblePlan(current, legacy) || current.Criteria[0].RequiresExpected == nil {
		t.Fatal("legacy offline binding changed or current metadata mutated")
	}
	legacy.Criteria[0].RequiresExpected = &optional
	if compatiblePlan(current, legacy) {
		t.Fatal("forged answer-independent capability accepted")
	}
}

func TestTaskCriteriaMergeRequirementsAcrossCases(t *testing.T) {
	yes, no := true, false
	a := []opt.CriterionDefinition{{ID: "contains", Required: true, RequiresExpected: &no}}
	b := []opt.CriterionDefinition{{ID: "contains", Required: true, RequiresExpected: &yes}}
	merged, err := mergeCriteria(a, b)
	if err != nil || merged[0].RequiresExpected == nil || !*merged[0].RequiresExpected || *a[0].RequiresExpected {
		t.Fatalf("mixed case requirements lost: %+v %v", merged, err)
	}
	b[0].Required = false
	if _, err = mergeCriteria(a, b); err == nil {
		t.Fatal("different required gates merged")
	}
}
