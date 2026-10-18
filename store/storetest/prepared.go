package storetest

import (
	"bytes"
	"reflect"
	"testing"
	"time"

	"github.com/xraph/cortex/execution"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/safety"
	"github.com/xraph/cortex/store"
)

func testPreparedRoundTrip(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx := ctxWithScope("prepared")
	r := mustCreateRun(t, s, ctx)
	r.Provenance = run.ExecutionProvenance{PublicationStatus: safety.StatusRedacted, CandidateID: "candidate-1", EnvironmentReceiptID: "environment-1", SchemaVersion: 1, PreparedDigest: "digest", Purpose: run.PurposeEvaluation, ModelRevision: "model-v1", Tools: []run.Revision{{Name: "probe", Revision: "tool-v1"}}, Diagnostics: []string{"optional missing"}}
	r.Budget = execution.Budget{MaxSteps: 5, MaxTokens: 20, MaxToolCalls: 2, Deadline: time.Now().UTC().Truncate(time.Millisecond).Add(time.Hour)}
	r.Usage = execution.Usage{Steps: 2, Tokens: 15, ReportedTokens: 11, ToolCalls: 1, Estimated: true}
	r.StepCount = 2
	r.TokensUsed = 15
	r.State = run.StatePaused
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Provenance, r.Provenance) || got.Usage != r.Usage || !got.Budget.Equal(r.Budget) {
		t.Fatalf("prepared accounting lost: %+v", got)
	}
	filter := &run.ListFilter{State: run.StatePaused}
	listed, err := s.ListRuns(ctx, filter)
	if err != nil || len(listed) != 1 || listed[0].State != run.StatePaused {
		t.Fatalf("paused list: %+v %v", listed, err)
	}
	count, err := s.CountRuns(ctx, filter)
	if err != nil || count != 1 {
		t.Fatalf("paused count: %d %v", count, err)
	}
	susp := newSuspension(r.ID)
	susp.Cont.Config.Budget = r.Budget
	susp.Cont.SchemaVersion = 2
	susp.Cont.Config.Publication = safety.Policy{Version: 1, Mode: safety.ModeBuffered, Required: true, MaxInputBytes: 1024, MaxOutputBytes: 2048, MaxToolOutputBytes: 512}
	susp.Cont.Prepared = []byte(`{"z":1,"a":2}`)
	susp.Cont.Provenance = r.Provenance
	susp.Cont.Usage = r.Usage
	if err = s.CreateSuspension(ctx, susp); err != nil {
		t.Fatal(err)
	}
	claimed, err := s.ClaimSuspension(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.Cont.Config.Publication != susp.Cont.Config.Publication || !bytes.Equal(claimed.Cont.Prepared, susp.Cont.Prepared) || claimed.Cont.Usage != r.Usage || !claimed.Cont.Config.Budget.Equal(r.Budget) || !reflect.DeepEqual(claimed.Cont.Provenance, r.Provenance) {
		t.Fatalf("prepared continuation lost: %+v", claimed.Cont)
	}
}
