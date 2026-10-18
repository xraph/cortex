package canonical

import (
	"encoding/json"
	"math"
	"testing"
)

func TestMarshalPreservesOrderAndExactNumbers(t *testing.T) {
	first, err := Marshal(map[string]any{"z": json.Number("9007199254740993"), "a": []string{"first", "second"}})
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != `{"a":["first","second"],"z":9007199254740993}` {
		t.Fatalf("lost canonical content: %s", first)
	}
	second, err := Marshal(map[string]any{"a": []string{"second", "first"}, "z": json.Number("9007199254740993")})
	if err != nil {
		t.Fatal(err)
	}
	if string(first) == string(second) {
		t.Fatal("section order disappeared")
	}
	if _, err = Marshal(math.NaN()); err == nil {
		t.Fatal("nonfinite value accepted")
	}
}

func TestDecodeRejectsAmbiguousOrUnknownMembers(t *testing.T) {
	type child struct {
		Text string `json:"text"`
	}
	type document struct {
		Sections []child `json:"sections"`
	}
	for _, raw := range []string{
		`{"sections":[],"sections":[]}`,
		`{"sections":[{"text":"first","text":"second"}]}`,
		`{"sections":[{"text":"ok","policy":"disable"}]}`,
		`{"sections":[],"budget":0}`,
		`{"sections":[]} {"sections":[]}`,
		`null`,
	} {
		t.Run(raw, func(t *testing.T) {
			var out document
			if err := Decode([]byte(raw), &out); err == nil {
				t.Fatal("ambiguous document accepted")
			}
		})
	}
	var out document
	if err := Decode([]byte(`{"sections":[{"text":"first"},{"text":"second"}]}`), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Sections) != 2 || out.Sections[1].Text != "second" {
		t.Fatalf("incorrect decode %+v", out)
	}
}
