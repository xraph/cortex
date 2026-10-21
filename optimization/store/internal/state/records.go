package state

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/internal/canonical"
)

// Record is one versioned, exact-scope database row. ID hashes the compound key;
// the original components remain available for equality checks and indexes.
type Record struct {
	Schema int
	Scope  string
	Kind   string
	Parent string
	Key    string
	ID     string
	Digest string
	Body   []byte
}

// Mutation updates one row. An empty ExpectedDigest requires an absent row.
// Database adapters must apply every mutation in the enclosing transaction.
type Mutation struct {
	Record         Record
	ExpectedDigest string
}

func digestParts(parts ...string) string {
	var input []byte
	for _, part := range parts {
		input = binary.AppendUvarint(input, uint64(len(part)))
		input = append(input, part...)
	}
	digest := sha256.Sum256(input)
	return hex.EncodeToString(digest[:])
}

func sealRecord(record Record) Record {
	record.ID = (RecordKey{Kind: record.Kind, Parent: record.Parent, Key: record.Key}).ID()
	record.Digest = digestParts(strconv.Itoa(record.Schema), record.Scope, record.Kind, record.Parent, record.Key, string(record.Body))
	return record
}

type recordCodec struct {
	kind   string
	encode func(*scopeData) ([]Record, error)
	decode func(*scopeData, Record) error
}

type recordValue[T any] struct {
	Value T `json:"value"`
}

func mapCodec[T any](kind string, field func(*scopeData) *map[string]T) recordCodec {
	return recordCodec{
		kind: kind,
		encode: func(data *scopeData) ([]Record, error) {
			return encodeMap(*field(data), "")
		},
		decode: func(data *scopeData, record Record) error {
			if record.Parent != "" {
				return optimization.ErrUnavailable
			}
			values := field(data)
			if *values == nil {
				*values = make(map[string]T)
			}
			return decodeMap(*values, record)
		},
	}
}

func nestedCodec[T any](kind string, field func(*scopeData) *map[string]map[string]T) recordCodec {
	return recordCodec{
		kind: kind,
		encode: func(data *scopeData) ([]Record, error) {
			var records []Record
			for parent, values := range *field(data) {
				rows, err := encodeMap(values, parent)
				if err != nil {
					return nil, err
				}
				records = append(records, rows...)
			}
			return records, nil
		},
		decode: func(data *scopeData, record Record) error {
			if record.Parent == "" {
				return optimization.ErrUnavailable
			}
			values := field(data)
			if *values == nil {
				*values = make(map[string]map[string]T)
			}
			inner := (*values)[record.Parent]
			if inner == nil {
				inner = make(map[string]T)
			}
			if err := decodeMap(inner, record); err != nil {
				return err
			}
			(*values)[record.Parent] = inner
			return nil
		},
	}
}

func encodeMap[T any](values map[string]T, parent string) ([]Record, error) {
	records := make([]Record, 0, len(values))
	for key, value := range values {
		body, err := canonical.Marshal(recordValue[T]{Value: value})
		if err != nil {
			return nil, err
		}
		records = append(records, Record{Parent: parent, Key: key, Body: body})
	}
	return records, nil
}

func decodeMap[T any](values map[string]T, record Record) error {
	var value recordValue[*T]
	if err := canonical.Decode(record.Body, &value); err != nil {
		return err
	}
	if value.Value == nil {
		return optimization.ErrUnavailable
	}
	if _, exists := values[record.Key]; exists {
		return optimization.ErrUnavailable
	}
	values[record.Key] = *value.Value
	return nil
}

var recordCodecs = []recordCodec{
	mapCodec("artifacts", func(s *scopeData) *map[string]optimization.ArtifactRetention { return &s.Artifacts }),
	mapCodec("experiments", func(s *scopeData) *map[string]optimization.Experiment { return &s.Experiments }),
	mapCodec("versions", func(s *scopeData) *map[string]optimization.HarnessVersion { return &s.Versions }),
	mapCodec("candidates", func(s *scopeData) *map[string]optimization.Candidate { return &s.Candidates }),
	mapCodec("trials", func(s *scopeData) *map[string]optimization.Trial { return &s.Trials }),
	mapCodec("evaluations", func(s *scopeData) *map[string]optimization.Evaluation { return &s.Evaluations }),
	mapCodec("decisions", func(s *scopeData) *map[string]optimization.Decision { return &s.Decisions }),
	mapCodec("allocations", func(s *scopeData) *map[string]optimization.ValidationAllocation { return &s.Allocations }),
	mapCodec("activations", func(s *scopeData) *map[string]optimization.Activation { return &s.Activations }),
	mapCodec("activation_heads", func(s *scopeData) *map[string]id.ID { return &s.ActivationHeads }),
	mapCodec("lessons", func(s *scopeData) *map[string]optimization.Lesson { return &s.Lessons }),
	mapCodec("publications", func(s *scopeData) *map[string]optimization.Publication { return &s.Publications }),
	mapCodec("publication_requests", func(s *scopeData) *map[string]optimization.PublicationRequest { return &s.PublicationRequests }),
	mapCodec("revocations", func(s *scopeData) *map[string]optimization.LessonRevocation { return &s.Revocations }),
	nestedCodec("transitions", func(s *scopeData) *map[string]map[string]string { return &s.Transitions }),
	nestedCodec("receipts", func(s *scopeData) *map[string]map[string]string { return &s.Receipts }),
	nestedCodec("reservations", func(s *scopeData) *map[string]map[string]optimization.Reservation { return &s.Reservations }),
	{kind: "events", encode: encodeEvents, decode: decodeEvent},
}

func encodeEvents(data *scopeData) ([]Record, error) {
	var records []Record
	for parent, events := range data.Events {
		for _, event := range events {
			body, err := canonical.Marshal(recordValue[optimization.Event]{Value: event})
			if err != nil {
				return nil, err
			}
			records = append(records, Record{Parent: parent, Key: strconv.FormatInt(event.Sequence, 10), Body: body})
		}
	}
	return records, nil
}

func decodeEvent(data *scopeData, record Record) error {
	var value recordValue[*optimization.Event]
	if err := canonical.Decode(record.Body, &value); err != nil {
		return err
	}
	if value.Value == nil {
		return optimization.ErrUnavailable
	}
	event := *value.Value
	if event.Sequence < 1 || record.Key != strconv.FormatInt(event.Sequence, 10) || record.Parent != event.ExperimentID.String() {
		return optimization.ErrUnavailable
	}
	data.Events[record.Parent] = append(data.Events[record.Parent], event)
	return nil
}

func encodeRecords(scope string, data *scopeData) ([]Record, error) {
	var records []Record
	for _, codec := range recordCodecs {
		rows, err := codec.encode(data)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			row.Schema, row.Scope, row.Kind = 1, scope, codec.kind
			records = append(records, sealRecord(row))
		}
	}
	slices.SortFunc(records, func(a, b Record) int { return strings.Compare(a.ID, b.ID) })
	return records, nil
}

func decodeRecords(scope string, records []Record) (*scopeData, error) {
	data := newScopeData()
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		sealed := sealRecord(record)
		if record.Schema != 1 || record.Scope != scope || record.Key == "" || record.ID != sealed.ID || record.Digest != sealed.Digest || seen[record.ID] {
			return nil, fmt.Errorf("%w: invalid stored optimization record", optimization.ErrUnavailable)
		}
		seen[record.ID] = true
		index := slices.IndexFunc(recordCodecs, func(codec recordCodec) bool { return codec.kind == record.Kind })
		if index < 0 {
			return nil, fmt.Errorf("%w: unknown stored optimization record kind", optimization.ErrUnavailable)
		}
		if err := recordCodecs[index].decode(data, record); err != nil {
			return nil, fmt.Errorf("%w: decode stored optimization record: %w", optimization.ErrUnavailable, err)
		}
	}
	for parent, events := range data.Events {
		slices.SortFunc(events, func(a, b optimization.Event) int {
			if a.Sequence < b.Sequence {
				return -1
			}
			if a.Sequence > b.Sequence {
				return 1
			}
			return 0
		})
		for i, event := range events {
			if event.Sequence != int64(i)+1 {
				return nil, fmt.Errorf("%w: stored audit sequence gap", optimization.ErrUnavailable)
			}
		}
		data.Events[parent] = events
	}
	return data, nil
}

func recordChanges(before, after []Record) ([]Mutation, error) {
	previous := make(map[string]Record, len(before))
	for _, record := range before {
		previous[record.ID] = record
	}
	changes := make([]Mutation, 0)
	for _, record := range after {
		old := previous[record.ID]
		if old.Digest != record.Digest {
			changes = append(changes, Mutation{Record: record, ExpectedDigest: old.Digest})
		}
		delete(previous, record.ID)
	}
	if len(previous) != 0 {
		return nil, fmt.Errorf("%w: optimization history cannot be removed", optimization.ErrConflict)
	}
	return changes, nil
}
