package run_test

import (
	"encoding/json"
	"testing"

	"github.com/xraph/cortex/run"
)

func TestHistoricalCompletionHasUnknownReason(t *testing.T) {
	var r run.Run
	if err := json.Unmarshal([]byte(`{"state":"completed","output":"old answer"}`), &r); err != nil {
		t.Fatal(err)
	}
	if r.State != run.StateCompleted || r.StopReason != "" {
		t.Fatalf("historical result gained unsupported evidence: %+v", r)
	}
}

func TestVerificationStatusCompatibility(t *testing.T) {
	for _, status := range []string{"", "passed", "future_status"} {
		var r run.Run
		payload := `{"state":"completed","verification_status":"` + status + `"}`
		if err := json.Unmarshal([]byte(payload), &r); err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err = json.Unmarshal(encoded, &fields); err != nil {
			t.Fatal(err)
		}
		want := status
		if want == "" {
			want = "unchecked"
		}
		if fields["verification_status"] != want {
			t.Errorf("status roundtrip = %v, want %s", fields["verification_status"], want)
		}
	}
}

func TestUnknownOutcomeValuesSurviveRoundTrip(t *testing.T) {
	var r run.Run
	if err := json.Unmarshal([]byte(`{"state":"future_state","stop_reason":"future_reason","verification_status":"future_status"}`), &r); err != nil {
		t.Fatal(err)
	}
	if string(r.State) != "future_state" || string(r.StopReason) != "future_reason" || string(r.VerificationStatus) != "future_status" {
		t.Fatalf("unknown values lost: %+v", r)
	}
}
