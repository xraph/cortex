package storetest

import (
	"errors"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

// CreateRunDuplicateID proves a second CreateRun with an id that already
// exists is refused with cortex.ErrAlreadyExists and leaves the first run
// as it was. The engine relies on it to make a host's pre-assigned run id
// idempotent: a retried start must not run the same work twice.
func CreateRunDuplicateID(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	ctx := ctxWithScope("run-duplicate")
	first := mustCreateRun(t, s, ctx)

	again := newRun(first.AgentID)
	again.ID = first.ID
	again.Input = "second"
	again.State = run.StateRunning
	err := s.CreateRun(ctx, again)
	if !errors.Is(err, cortex.ErrAlreadyExists) {
		t.Fatalf("second CreateRun with the same id = %v, want ErrAlreadyExists", err)
	}

	got, err := s.GetRun(ctx, first.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Input != first.Input || got.State != first.State || got.Revision != first.Revision {
		t.Fatalf("after the refused duplicate the run is input %q state %q revision %d, want %q %q %d", got.Input, got.State, got.Revision, first.Input, first.State, first.Revision)
	}

	// The id is taken in every scope, not only the one that holds it.
	other := ctxWithScope("run-duplicate-elsewhere")
	elsewhere := newRun(mustCreateAgent(t, s, other, ""))
	elsewhere.ID = first.ID
	if err := s.CreateRun(other, elsewhere); !errors.Is(err, cortex.ErrAlreadyExists) {
		t.Fatalf("CreateRun with the same id in another scope = %v, want ErrAlreadyExists", err)
	}
}
