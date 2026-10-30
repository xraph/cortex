package suspension_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/suspension"
)

// A progress snapshot travels as JSON. The prepared bytes go as base64, so
// a database that reorders JSON object keys cannot change them.
func TestProgressJSONKeepsPreparedBytes(t *testing.T) {
	p := suspension.Progress{
		RunID: id.NewAgentRunID(),
		Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}},
		Owner: "w1",
		Cont: suspension.Continuation{
			Prepared: []byte(`{"z": 1,  "a":2}`),
			Messages: []llm.Message{{Role: "user", Content: "hello"}},
			Config:   suspension.RunConfig{MaxSteps: 3},
		},
		Started:   []suspension.PendingCall{{ID: "call_1", Name: "deploy", Arguments: `{"env":"prod"}`}},
		Done:      []suspension.ProgressResult{{CallID: "call_0", Content: "ok", Outcome: "completed"}},
		UpdatedAt: time.Now().UTC().Truncate(time.Millisecond),
	}
	data, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), `"z": 1`) {
		t.Fatalf("prepared bytes were embedded as JSON, not base64: %s", data)
	}
	var back suspension.Progress
	if err = json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, p) {
		t.Fatalf("round trip changed the snapshot:\n%+v\n%+v", back, p)
	}
	for _, key := range []string{`"run_id"`, `"owner"`, `"continuation"`, `"started"`, `"done"`, `"updated_at"`, `"call_id"`, `"outcome"`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("encoded snapshot has no %s key: %s", key, data)
		}
	}
}
