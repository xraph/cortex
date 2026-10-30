package storetest

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
)

// RunReplyRoute proves a run keeps the reply route it was created with, and
// never serializes it. The engine writes a directive run's route, the
// asker's reply token included, so that whichever replica finishes the run
// can answer; a run is served to API clients as JSON, and the token must
// not reach them.
func RunReplyRoute(t *testing.T, newStore func(t *testing.T) store.Store) {
	s := newStore(t)
	ctx := ctxWithScope("run-reply-route")
	want := []byte(`{"in_reply_to":"reply-token-route"}`)

	r := newRun(mustCreateAgent(t, s, ctx, ""))
	r.ReplyRoute = append([]byte(nil), want...)
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if !bytes.Equal(got.ReplyRoute, want) {
		t.Fatalf("GetRun reply route = %q, want %q", got.ReplyRoute, want)
	}

	// An update never rewrites it, with or without a route of its own.
	got.State = run.StatePaused
	got.ReplyRoute = nil
	if err = s.UpdateRun(ctx, got); err != nil {
		t.Fatalf("UpdateRun: %v", err)
	}
	kept, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("GetRun after UpdateRun: %v", err)
	}
	if kept.State != run.StatePaused || !bytes.Equal(kept.ReplyRoute, want) {
		t.Fatalf("after UpdateRun: state %q route %q, want %q and %q", kept.State, kept.ReplyRoute, run.StatePaused, want)
	}
	kept.ReplyRoute = []byte(`{"in_reply_to":"forged"}`)
	if err = s.UpdateRun(ctx, kept); err != nil {
		t.Fatalf("UpdateRun with another route: %v", err)
	}
	if again, readErr := s.GetRun(ctx, r.ID); readErr != nil || !bytes.Equal(again.ReplyRoute, want) {
		t.Fatalf("route after an update carrying another = %q, %v; want %q", again.ReplyRoute, readErr, want)
	}

	listed, err := s.ListRuns(ctx, &run.ListFilter{Exact: true})
	if err != nil || len(listed) != 1 || !bytes.Equal(listed[0].ReplyRoute, want) {
		t.Fatalf("ListRuns reply route = %+v, %v; want one run carrying %q", listed, err, want)
	}

	encoded, err := json.Marshal(listed[0])
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	if strings.Contains(string(encoded), "reply-token-route") || strings.Contains(string(encoded), "reply_route") {
		t.Errorf("run JSON carries the reply route: %s", encoded)
	}
}
