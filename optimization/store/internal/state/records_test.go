package state

import (
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

func TestRecordsKeepCompoundIdentitiesAndEventOrder(t *testing.T) {
	data := newScopeData()
	data.Transitions["a/b"] = map[string]string{"c": "first"}
	data.Transitions["a"] = map[string]string{"b/c": "second"}
	data.Receipts["a"] = map[string]string{"b/c": "receipt"}
	experimentID := id.New(optimization.PrefixExperiment)
	for i := 1; i <= 12; i++ {
		data.Events[experimentID.String()] = append(data.Events[experimentID.String()], optimization.Event{ID: id.New(optimization.PrefixEvent), ExperimentID: experimentID, Sequence: int64(i), Type: "event", Payload: []byte(`{}`)})
	}
	records, err := encodeRecords("workspace=one", data)
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 15 {
		t.Fatalf("records = %d, want 15 individual records", len(records))
	}
	slices.Reverse(records)
	decoded, err := decodeRecords("workspace=one", records)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(data, decoded) {
		t.Fatal("compound identities or event order changed")
	}
}

func TestRecordsRejectCorruption(t *testing.T) {
	data := newScopeData()
	data.Transitions["experiment"] = map[string]string{"request": "digest"}
	records, err := encodeRecords("workspace=one", data)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		change func([]Record) []Record
	}{
		{"scope", func(r []Record) []Record { r[0].Scope = "workspace=two"; return r }},
		{"body", func(r []Record) []Record { r[0].Body = []byte(`{"value":"changed"}`); return r }},
		{"key", func(r []Record) []Record { r[0].Key = "other"; return r }},
		{"parent", func(r []Record) []Record { r[0].Parent = "other"; return r }},
		{"unknown kind", func(r []Record) []Record { r[0].Kind = "future"; r[0] = sealRecord(r[0]); return r }},
		{"unknown schema", func(r []Record) []Record { r[0].Schema = 2; r[0] = sealRecord(r[0]); return r }},
		{"duplicate", func(r []Record) []Record { return append(r, r[0]) }},
		{"missing value", func(r []Record) []Record { r[0].Body = []byte(`{}`); r[0] = sealRecord(r[0]); return r }},
		{"null value", func(r []Record) []Record { r[0].Body = []byte(`{"value":null}`); r[0] = sealRecord(r[0]); return r }},
		{"unknown field", func(r []Record) []Record {
			r[0].Body = []byte(`{"value":"digest","extra":true}`)
			r[0] = sealRecord(r[0])
			return r
		}},
		{"duplicate field", func(r []Record) []Record {
			r[0].Body = []byte(`{"value":"digest","value":"different"}`)
			r[0] = sealRecord(r[0])
			return r
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, decodeErr := decodeRecords("workspace=one", tc.change(slices.Clone(records)))
			if !errors.Is(decodeErr, optimization.ErrUnavailable) {
				t.Fatalf("corrupt record accepted: %v", decodeErr)
			}
		})
	}
}

func TestRecordDiffWritesOnlyChangedRows(t *testing.T) {
	data := newScopeData()
	data.Transitions["one"] = map[string]string{"first": "old", "second": "unchanged"}
	before, err := encodeRecords("workspace=one", data)
	if err != nil {
		t.Fatal(err)
	}
	data.Transitions["one"]["first"] = "new"
	data.Transitions["one"]["third"] = "added"
	after, err := encodeRecords("workspace=one", data)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := recordChanges(before, after)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("writes = %d, want 2", len(changes))
	}
	byKey := make(map[string]Mutation)
	for _, change := range changes {
		byKey[change.Record.Key] = change
	}
	if byKey["first"].ExpectedDigest == "" || byKey["third"].ExpectedDigest != "" {
		t.Fatal("lost update/insert preconditions")
	}
	if _, err = recordChanges(after, before); !errors.Is(err, optimization.ErrConflict) {
		t.Fatalf("audit row deletion accepted: %v", err)
	}
}
