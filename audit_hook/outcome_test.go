package audithook

import (
	"context"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/plugin"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/verification"
)

func TestAuditRecordsIncompleteOutcome(t *testing.T) {
	var events []*AuditEvent
	ext := New(RecorderFunc(func(_ context.Context, event *AuditEvent) error { events = append(events, event); return nil }))
	registry := plugin.NewRegistry(nil)
	registry.Register(ext)
	registry.EmitRunTerminated(context.Background(), id.NewAgentID(), id.NewAgentRunID(), run.TerminalSummary{State: run.StateIncomplete, StopReason: run.StopPolicyUnavailable, VerificationStatus: verification.StatusUnchecked})
	if len(events) != 1 {
		t.Fatalf("missing audit outcome: %v", events)
	}
	event := events[0]
	if event.Outcome != OutcomeFailure || event.Metadata["stop_reason"] != "policy_unavailable" || event.Metadata["verification_status"] != "unchecked" {
		t.Fatalf("wrong audit outcome: %+v", event)
	}
	if _, ok := event.Metadata["output"]; ok {
		t.Fatal("raw output included in outcome audit")
	}
}
