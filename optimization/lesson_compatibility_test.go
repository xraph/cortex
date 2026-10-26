package optimization

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/learning"
)

func TestSharedLessonPreservesOptimizationSerialization(t *testing.T) {
	value := Lesson{ID: id.New(PrefixLesson), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "project", Value: "one"}}}, Domain: "reasoning", HarnessSchema: 1, Statement: "Check retained evidence", EvidenceClass: EvidenceValidated, Revision: 1, CreatedAt: time.Date(2026, 10, 26, 0, 0, 0, 0, time.UTC)}
	shared := func(v learning.Lesson) learning.Lesson { return v }(value)
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	got, err := json.Marshal(shared)
	if err != nil || string(got) != string(encoded) {
		t.Fatal("shared lesson changed serialization", err)
	}
	expected := `{"id":"` + value.ID.String() + `","scope":{"levels":[{"key":"project","value":"one"}]},"domain":"reasoning","harness_schema":1,"statement":"Check retained evidence","applicability":null,"contraindications":null,"decision_ids":null,"trial_ids":null,"evidence_class":"validated","revision":1,"created_at":"2026-10-26T00:00:00Z","expires_at":"0001-01-01T00:00:00Z","revoked_at":"0001-01-01T00:00:00Z"}`
	if string(encoded) != expected {
		t.Fatalf("legacy JSON changed: %s", encoded)
	}
	oldDigest, err := LessonDigest(value)
	if err != nil {
		t.Fatal(err)
	}
	newDigest, err := learning.Digest(shared)
	if err != nil || oldDigest != newDigest {
		t.Fatal("legacy digest changed", err)
	}
}
