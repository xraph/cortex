package optimization_test

import (
	"errors"
	"testing"

	"github.com/xraph/cortex/optimization"
)

func TestTrialExecutorRequiresQualifiedCapabilities(t *testing.T) {
	for _, mode := range []string{"isolated", "recoverable", "stoppable", "atomic"} {
		caps := optimization.IsolationCapabilities{Isolated: true, Resettable: true, Recoverable: true, Stoppable: true, FrozenRetrieval: true, AtomicSources: true, DeterministicApproval: true}
		switch mode {
		case "isolated":
			caps.Isolated = false
		case "recoverable":
			caps.Recoverable = false
		case "stoppable":
			caps.Stoppable = false
		case "atomic":
			caps.AtomicSources = false
		}
		if err := optimization.ValidateTrialCapabilities(caps, true); !errors.Is(err, optimization.ErrUnavailable) {
			t.Fatalf("accepted missing %s: %v", mode, err)
		}
	}
}
