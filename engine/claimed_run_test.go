package engine

import (
	"context"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

type noClaimReadStore struct{ store.Store }

func (noClaimReadStore) GetRun(context.Context, id.AgentRunID) (*run.Run, error) {
	panic("claim snapshot must not require another store read")
}

func TestClaimedRunUsesAtomicSnapshot(t *testing.T) {
	e := &Engine{store: noClaimReadStore{}}
	runID := id.NewAgentRunID()
	before := &run.Run{ID: runID, Revision: 3, State: run.StatePaused}
	claimed := &run.Run{ID: runID, Revision: 5, State: run.StateRunning, Output: "changed before claim"}
	got := e.claimedRun(t.Context(), runID, before, claimed)
	if got != claimed || got.Revision != 5 || got.Output != claimed.Output {
		t.Fatalf("used pre-claim state: %+v", got)
	}
}
