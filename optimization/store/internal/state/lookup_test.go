package state

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
)

type lookupBackend struct {
	Backend
	want    RecordKey
	records []Record
	calls   int
}

func (b *lookupBackend) Load(context.Context, string) ([]Record, error) {
	return nil, errors.New("point read loaded whole scope")
}

func (b *lookupBackend) LoadRecord(_ context.Context, _ string, key RecordKey) ([]Record, error) {
	b.calls++
	if key != b.want {
		return nil, errors.New("point read requested wrong compound identity")
	}
	return b.records, nil
}

func TestPointReadersRequestOneIdentity(t *testing.T) {
	entity := id.New(optimization.PrefixExperiment)
	artifact := optimization.ArtifactRef{ExperimentID: entity, Scope: cortex.ScopeFromContext(operationContext("lookup")), Digest: strings.Repeat("a", 64), Size: 1, MediaType: "text/plain", Kind: "trace", AccessPolicy: "restricted"}
	artifactBinding, err := artifact.BindingDigest()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name string
		key  RecordKey
		read func(context.Context, *Store) error
	}{
		{"artifact", RecordKey{Kind: "artifacts", Key: artifactBinding}, func(ctx context.Context, s *Store) error { _, err := s.GetArtifact(ctx, artifact); return err }},
		{"creation", RecordKey{Kind: "creation_receipts", Key: "operator/" + strings.Repeat("a", 64)}, func(ctx context.Context, s *Store) error {
			_, err := s.GetCreationReceipt(ctx, "operator/"+strings.Repeat("a", 64))
			return err
		}},
		{"experiment", RecordKey{Kind: "experiments", Key: entity.String()}, func(ctx context.Context, s *Store) error { _, err := s.GetExperiment(ctx, entity); return err }},
		{"version", RecordKey{Kind: "versions", Key: entity.String()}, func(ctx context.Context, s *Store) error { _, err := s.GetVersion(ctx, entity); return err }},
		{"candidate", RecordKey{Kind: "candidates", Key: entity.String()}, func(ctx context.Context, s *Store) error { _, err := s.GetCandidate(ctx, entity); return err }},
		{"trial", RecordKey{Kind: "trials", Key: entity.String()}, func(ctx context.Context, s *Store) error { _, err := s.GetTrial(ctx, entity); return err }},
		{"evaluation", RecordKey{Kind: "evaluations", Key: "job/a/b"}, func(ctx context.Context, s *Store) error { _, err := s.GetEvaluation(ctx, "job/a/b"); return err }},
		{"decision", RecordKey{Kind: "decisions", Key: entity.String()}, func(ctx context.Context, s *Store) error { _, err := s.GetDecision(ctx, entity); return err }},
		{"allocation", RecordKey{Kind: "allocations", Key: entity.String()}, func(ctx context.Context, s *Store) error {
			_, err := s.GetValidationAllocation(ctx, entity)
			return err
		}},
		{"reservation", RecordKey{Kind: "reservations", Parent: entity.String(), Key: "call/a/b"}, func(ctx context.Context, s *Store) error {
			_, err := s.GetReservation(ctx, entity, "call/a/b")
			return err
		}},
		{"lesson", RecordKey{Kind: "lessons", Key: entity.String()}, func(ctx context.Context, s *Store) error { _, err := s.GetLesson(ctx, entity); return err }},
		{"publication", RecordKey{Kind: "publications", Key: publicationKey(entity, 2)}, func(ctx context.Context, s *Store) error { _, err := s.GetPublication(ctx, entity, 2); return err }},
		{"activation-generation", RecordKey{Kind: "activations", Key: entity.String()}, func(ctx context.Context, s *Store) error {
			_, err := s.GetActivationGeneration(ctx, entity)
			return err
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &lookupBackend{want: test.key}
			s := NewBackend(backend)
			if err := test.read(operationContext("lookup"), s); !errors.Is(err, optimization.ErrNotFound) || backend.calls != 1 {
				t.Fatalf("lookup: %v, point calls %d", err, backend.calls)
			}
			if err := test.read(context.Background(), s); !errors.Is(err, cortex.ErrNoScope) || backend.calls != 1 {
				t.Fatalf("unscoped lookup: %v, point calls %d", err, backend.calls)
			}
		})
	}
}

func TestPointReaderRejectsUnexpectedRecords(t *testing.T) {
	ctx := operationContext("lookup-integrity")
	scope, err := contextScope(ctx)
	if err != nil {
		t.Fatal(err)
	}
	entity := id.New(optimization.PrefixExperiment)
	key := RecordKey{Kind: "experiments", Key: entity.String()}
	data := newScopeData()
	data.Experiments[key.Key] = optimization.Experiment{ID: entity}
	records, err := encodeRecords(scope, data)
	if err != nil || len(records) != 1 {
		t.Fatalf("encode fixture: %v", err)
	}
	backend := &lookupBackend{want: key, records: records}
	got, err := NewBackend(backend).GetExperiment(ctx, entity)
	if err != nil || got.ID != entity {
		t.Fatalf("valid point record: %v, %v", got.ID, err)
	}
	for _, test := range []struct {
		name string
		edit func(Record) []Record
	}{
		{"duplicate", func(r Record) []Record { return []Record{r, r} }},
		{"wrong-key", func(r Record) []Record { r.Key = "other"; return []Record{sealRecord(r)} }},
		{"wrong-scope", func(r Record) []Record { r.Scope = "workspace=foreign"; return []Record{sealRecord(r)} }},
		{"future-schema", func(r Record) []Record { r.Schema = 2; return []Record{sealRecord(r)} }},
		{"changed-content", func(r Record) []Record { r.Body = []byte(`{}`); return []Record{r} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := &lookupBackend{want: key, records: test.edit(records[0])}
			if _, err := NewBackend(backend).GetExperiment(ctx, entity); !errors.Is(err, optimization.ErrUnavailable) {
				t.Fatalf("invalid point record admitted: %v", err)
			}
		})
	}
}
