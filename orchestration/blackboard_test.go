package orchestration_test

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/orchestration"
)

func TestBlackboardReadWrite(t *testing.T) {
	bb := orchestration.NewBlackboard(id.NewOrchestrationID(), nil, nil)
	bb.Write("k", "v")
	got, ok := bb.Read("k")
	if !ok || got != "v" {
		t.Fatalf("Read = %v, %v; want v, true", got, ok)
	}
	if _, ok := bb.Read("missing"); ok {
		t.Fatal("expected missing key to report ok=false")
	}
}

func TestBlackboardSnapshotAndRoster(t *testing.T) {
	parts := []orchestration.Participant{
		{AgentName: "writer", Role: "author"},
		{AgentName: "editor", Role: "critic"},
	}
	bb := orchestration.NewBlackboard(id.NewOrchestrationID(), parts, nil)
	bb.Append("writer", "first draft")
	snap := bb.Snapshot()
	if !strings.Contains(snap, "writer") || !strings.Contains(snap, "first draft") {
		t.Fatalf("snapshot missing contribution: %q", snap)
	}
	if !strings.Contains(snap, "editor") {
		t.Fatalf("snapshot missing roster member: %q", snap)
	}
	if len(bb.Roster()) != 2 {
		t.Fatalf("roster len = %d, want 2", len(bb.Roster()))
	}
}

func TestBlackboardHandoffFiresCallback(t *testing.T) {
	var got orchestration.Handoff
	called := 0
	cb := func(_ context.Context, h orchestration.Handoff) {
		called++
		got = h
	}
	bb := orchestration.NewBlackboard(id.NewOrchestrationID(), nil, cb)
	bb.Handoff(context.Background(), "writer", "editor", "draft")
	if called != 1 {
		t.Fatalf("callback called %d times, want 1", called)
	}
	if got.From != "writer" || got.To != "editor" || got.Payload != "draft" {
		t.Fatalf("handoff = %+v", got)
	}
	if len(bb.Handoffs()) != 1 {
		t.Fatalf("recorded handoffs = %d, want 1", len(bb.Handoffs()))
	}
}

func TestBlackboardConcurrentAccess(t *testing.T) {
	bb := orchestration.NewBlackboard(id.NewOrchestrationID(), nil, nil)
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			bb.Write("k", n)
			bb.Append("agent", "msg")
			_, _ = bb.Read("k")
			_ = bb.Snapshot()
		}(i)
	}
	wg.Wait()
	if len(bb.Entries()) != 50 {
		t.Fatalf("entries = %d, want 50", len(bb.Entries()))
	}
}
