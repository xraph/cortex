package storetest

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

// RunPrincipal proves a run keeps the codec bytes it was created with. A
// resume decodes them to put the original caller back on the context, so
// a store that dropped them on any read, or on the update that pauses
// the run, would resume every run under nobody.
//
// The bytes never reach JSON: a run is served to API clients as JSON, and
// what a host's codec wrote is the host's business.
func RunPrincipal(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	ctx := ctxWithScope("run-principal")
	want := []byte{0x01, 0x02}

	r := newRun(mustCreateAgent(t, s, ctx, ""))
	r.Principal = append([]byte(nil), want...)
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !bytes.Equal(got.Principal, want) {
		t.Fatalf("GetRun principal = %v, want %v", got.Principal, want)
	}

	got.State = run.StatePaused
	if err = s.UpdateRun(ctx, got); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	updated, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun after UpdateRun: %v", err)
	}
	if updated.State != run.StatePaused || !bytes.Equal(updated.Principal, want) {
		t.Fatalf("after UpdateRun: state %q principal %v, want %q and %v", updated.State, updated.Principal, run.StatePaused, want)
	}

	// An update that carries no principal leaves the stored one alone. The
	// bytes are written once, when the run is created, and every later
	// writer works from a run it read back.
	updated.Principal = nil
	updated.Output = "changed"
	if err = s.UpdateRun(ctx, updated); err != nil {
		t.Fatalf("UpdateRun without principal: %v", err)
	}
	kept, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun after principal-less UpdateRun: %v", err)
	}
	if !bytes.Equal(kept.Principal, want) {
		t.Fatalf("principal after an update without one = %v, want %v", kept.Principal, want)
	}

	susp := newSuspension(r.ID)
	if err = s.CreateSuspension(ctx, susp); err != nil {
		t.Fatalf("CreateSuspension: %v", err)
	}
	claimed, err := s.ClaimSuspension(ctx, r.ID)
	if err != nil {
		t.Fatalf("ClaimSuspension: %v", err)
	}
	if claimed.ClaimedRun == nil || !bytes.Equal(claimed.ClaimedRun.Principal, want) {
		t.Fatalf("claimed run = %+v, want one carrying principal %v", claimed.ClaimedRun, want)
	}

	listed, err := s.ListRuns(ctx, &run.ListFilter{Exact: true})
	if err != nil || len(listed) != 1 || !bytes.Equal(listed[0].Principal, want) {
		t.Fatalf("ListRuns principal = %+v, %v; want one run carrying %v", listed, err, want)
	}

	encoded, err := json.Marshal(kept)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	var fields map[string]any
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal run: %v", err)
	}
	if _, present := fields["principal"]; present {
		t.Errorf("run JSON carries a principal key: %s", encoded)
	}
}
