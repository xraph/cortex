package optimization_test

import (
	"testing"

	"github.com/xraph/cortex/optimization"
)

func TestCriterionFailureKeyPreservesTupleIdentity(t *testing.T) {
	first := optimization.CriterionFailureKey("task", 1, "quality/2/safety")
	second := optimization.CriterionFailureKey("task/1/quality", 2, "safety")
	if first == second {
		t.Fatal("distinct required failures collapsed into one identity")
	}
	if first != optimization.CriterionFailureKey("task", 1, "quality/2/safety") {
		t.Fatal("failure identity is unstable")
	}
}
