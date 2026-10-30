package run_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

// A run's JSON is what API clients read. The lease expiry and the reclaim
// count say when a run's hold lapses and how often it was taken back, which
// an operator needs. The lease owner names the worker's host, the readiness
// carries tool results, the reply route carries a reply token and the
// notification time is sweeper bookkeeping, so none of those is ever
// serialized.
func TestRunJSONCarriesLeaseNotReadiness(t *testing.T) {
	at := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	r := run.Run{
		State:      run.StateReady,
		Lease:      &run.Lease{Owner: "w1", ExpiresAt: at},
		Ready:      &run.Readiness{Source: run.ReadyApproval, Results: json.RawMessage(`{"secret":"tool output"}`), CheckpointID: id.NewCheckpointID(), At: at},
		Reclaims:   2,
		NotifiedAt: &at,
		ReplyRoute: []byte(`{"in_reply_to":"token"}`),
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ready", "notified_at", "results", "reply_route", "owner"} {
		if _, present := fields[key]; present {
			t.Errorf("run JSON carries %q: %s", key, encoded)
		}
	}
	var decoded run.Run
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(fields["lease"]) == "" || json.Unmarshal(fields["lease"], &map[string]any{}) != nil {
		t.Errorf("run JSON lost its lease expiry: %s", encoded)
	}
	if decoded.State != run.StateReady || decoded.Lease == nil || decoded.Lease.Owner != "" || !decoded.Lease.ExpiresAt.Equal(at) || decoded.Reclaims != 2 {
		t.Fatalf("decoded run lost its lease or reclaims: %+v", decoded)
	}

	quiet, err := json.Marshal(run.Run{State: run.StateRunning})
	if err != nil {
		t.Fatal(err)
	}
	var quietFields map[string]json.RawMessage
	if err = json.Unmarshal(quiet, &quietFields); err != nil {
		t.Fatal(err)
	}
	if _, present := quietFields["lease"]; present {
		t.Errorf("a run with no lease serializes one: %s", quiet)
	}
}
