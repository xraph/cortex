package grpcbind

import (
	"testing"

	"github.com/xraph/cortex/a2aremote"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func TestTaskProjectionRetainsAssuranceMetadata(t *testing.T) {
	task := a2aremote.TaskFromRun(&run.Run{ID: id.NewAgentRunID(), State: run.StateIncomplete, StopReason: run.StopPolicyUnavailable}, "")
	protoTask := taskToProto(&task)
	if protoTask.Metadata == nil {
		t.Fatal("assurance metadata lost")
	}
	update := streamResponseOf(a2aremote.StreamEvent{StatusUpdate: &a2aremote.TaskStatusUpdateEvent{TaskID: task.ID, Status: task.Status, Metadata: task.Metadata, Final: true}})
	if update.GetStatusUpdate().Metadata == nil {
		t.Fatal("stream lost assurance metadata")
	}
	values := protoTask.Metadata.AsMap()
	summary, ok := values["cortex.assurance"].(map[string]any)
	if !ok || summary["verification_status"] != "unchecked" || summary["stop_reason"] != "policy_unavailable" {
		t.Fatalf("wrong metadata: %v", values)
	}
}
