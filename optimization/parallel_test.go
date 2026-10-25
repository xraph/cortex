package optimization

import (
	"slices"
	"testing"

	"github.com/xraph/cortex/id"
)

func TestParallelAdmissionRespectsUnknownAttempts(t *testing.T) {
	unknown := Trial{ID: id.New(PrefixTrial), Split: SplitDiscovery, State: TrialUnknown, Attempts: []Attempt{{ID: id.New(PrefixAttempt)}}}
	queued := Trial{ID: id.New(PrefixTrial), Split: SplitDiscovery, State: TrialQueued}
	batch, pending := parallelTrials([]Trial{queued, unknown}, PhaseBaseline, 1)
	if !pending || len(batch) != 1 || batch[0].ID != unknown.ID {
		t.Fatalf("unknown attempt lost its concurrency slot: %+v", batch)
	}
	// An unresolved attempt outside this phase still consumes experiment capacity.
	unknown.Split = SplitValidation
	batch, pending = parallelTrials([]Trial{queued, unknown}, PhaseBaseline, 1)
	if !pending || len(batch) != 0 {
		t.Fatalf("cross-phase unknown effect admitted a replacement: %+v", batch)
	}
}

func TestParallelAdmissionOrderIndependent(t *testing.T) {
	trials := []Trial{
		{ID: id.New(PrefixTrial), Split: SplitDiscovery, State: TrialQueued},
		{ID: id.New(PrefixTrial), Split: SplitDiscovery, State: TrialQueued},
		{ID: id.New(PrefixTrial), Split: SplitDiscovery, State: TrialQueued},
	}
	first, _ := parallelTrials(trials, PhaseBaseline, 2)
	slices.Reverse(trials)
	second, _ := parallelTrials(trials, PhaseBaseline, 2)
	if len(first) != 2 || len(second) != 2 || first[0].ID != second[0].ID || first[1].ID != second[1].ID {
		t.Fatalf("listing order changed bounded admission: %+v vs %+v", first, second)
	}
}
