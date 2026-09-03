package a2aremote

import (
	"encoding/json"
	"testing"
)

// The JSON names are the protocol's, not ours. A field renamed by a
// careless refactor is a peer that silently stops understanding us, so
// the wire shape is pinned here rather than trusted.
func TestMessageJSONNames(t *testing.T) {
	m := Message{
		MessageID: "msg_1", ContextID: "conv_1", TaskID: "arun_1",
		Role: RoleAgent, Parts: []Part{{Text: "hello"}},
		Extensions: []string{FIPAExtensionURI}, ReferenceTaskIDs: []string{"arun_2"},
		Metadata: map[string]any{"k": "v"},
	}
	b, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"messageId", "contextId", "taskId", "role", "parts", "extensions", "referenceTaskIds", "metadata"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing wire field %q in %s", key, b)
		}
	}
}

func TestTaskJSONNames(t *testing.T) {
	task := Task{
		ID: "arun_1", ContextID: "conv_1",
		Status:    TaskStatus{State: TaskStateWorking},
		Artifacts: []Artifact{{ArtifactID: "a1", Parts: []Part{{Text: "out"}}}},
	}
	b, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"id", "contextId", "status", "artifacts"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("missing wire field %q in %s", key, b)
		}
	}
	artifacts, _ := raw["artifacts"].([]any)
	first, _ := artifacts[0].(map[string]any)
	if _, ok := first["artifactId"]; !ok {
		t.Errorf("artifact is missing artifactId: %s", b)
	}
}

func TestTaskStateStringsAreTheProtocolsOwn(t *testing.T) {
	want := map[TaskState]string{
		TaskStateSubmitted:     "TASK_STATE_SUBMITTED",
		TaskStateWorking:       "TASK_STATE_WORKING",
		TaskStateCompleted:     "TASK_STATE_COMPLETED",
		TaskStateFailed:        "TASK_STATE_FAILED",
		TaskStateCanceled:      "TASK_STATE_CANCELED",
		TaskStateRejected:      "TASK_STATE_REJECTED",
		TaskStateInputRequired: "TASK_STATE_INPUT_REQUIRED",
		TaskStateAuthRequired:  "TASK_STATE_AUTH_REQUIRED",
	}
	if len(want) != 8 {
		t.Fatalf("the protocol defines 8 task states, table has %d", len(want))
	}
	for state, s := range want {
		if string(state) != s {
			t.Errorf("state = %q, want %q", state, s)
		}
	}
}

func TestTerminalStates(t *testing.T) {
	for _, s := range []TaskState{TaskStateCompleted, TaskStateFailed, TaskStateCanceled, TaskStateRejected} {
		if !s.Terminal() {
			t.Errorf("%s should be terminal", s)
		}
	}
	// An interrupted task is not finished: a client that stopped polling
	// on input-required would abandon a task that is about to continue.
	for _, s := range []TaskState{TaskStateSubmitted, TaskStateWorking, TaskStateInputRequired, TaskStateAuthRequired} {
		if s.Terminal() {
			t.Errorf("%s must not be terminal", s)
		}
	}
}

func TestRoleStrings(t *testing.T) {
	if RoleUser != "ROLE_USER" || RoleAgent != "ROLE_AGENT" {
		t.Fatalf("roles = %q/%q, want the protocol's enum names", RoleUser, RoleAgent)
	}
}
