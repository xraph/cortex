package artifacts_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/artifacts"
)

type readFunc func(context.Context, optimization.ArtifactRef) (optimization.Artifact, error)

func (f readFunc) Get(ctx context.Context, ref optimization.ArtifactRef) (optimization.Artifact, error) {
	return f(ctx, ref)
}

func TestRetainedReaderRefusesDeletionDuringProviderRead(t *testing.T) {
	s, ctx, req := fixture(t)
	value, err := s.GetArtifact(ctx, req.Ref)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("trace data")
	ref := req.Ref
	deleting := false
	calls := 0
	provider := readFunc(func(ctx context.Context, _ optimization.ArtifactRef) (optimization.Artifact, error) {
		calls++
		if deleting {
			deletion := req
			deletion.Ref = ref
			if deleteErr := s.Transact(ctx, func(tx optimization.Transaction) error { return tx.BeginArtifactDeletion(deletion) }); deleteErr != nil {
				t.Fatal(deleteErr)
			}
		}
		return optimization.Artifact{Ref: ref, Data: data, CreatedAt: value.CreatedAt, ExpiresAt: value.ExpiresAt}, nil
	})
	reader, err := artifacts.NewRetainedReader(s, provider, 1024, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Get(ctx, ref)
	if err != nil || string(got.Data) != string(data) {
		t.Fatalf("valid retained read: %v", err)
	}
	deleting = true
	got, err = reader.Get(ctx, ref)
	if !errors.Is(err, optimization.ErrIncompleteEvidence) || len(got.Data) != 0 {
		t.Fatalf("deletion during read leaked bytes: %v", err)
	}
	if _, err = reader.Get(ctx, ref); !errors.Is(err, optimization.ErrIncompleteEvidence) {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("pending deletion reached provider")
	}
}

func TestRetainedReaderRejectsChangedProviderRetention(t *testing.T) {
	s, ctx, req := fixture(t)
	value, err := s.GetArtifact(ctx, req.Ref)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("trace data")
	ref := req.Ref
	provider := readFunc(func(context.Context, optimization.ArtifactRef) (optimization.Artifact, error) {
		return optimization.Artifact{Ref: ref, Data: data, CreatedAt: value.CreatedAt, ExpiresAt: value.ExpiresAt.Add(time.Hour)}, nil
	})
	reader, err := artifacts.NewRetainedReader(s, provider, 1024, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reader.Get(ctx, ref)
	if !errors.Is(err, optimization.ErrIncompleteEvidence) || len(got.Data) != 0 {
		t.Fatalf("provider extended retention: %v", err)
	}
}

func TestRetainedReaderRejectsBeforeProvider(t *testing.T) {
	for _, mode := range []string{"missing", "expired", "unscoped", "foreign", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			s, ctx, req := fixture(t)
			value, err := s.GetArtifact(ctx, req.Ref)
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now()
			switch mode {
			case "missing":
				req.Ref.Kind = "not-registered"
			case "expired":
				now = value.ExpiresAt
			case "unscoped":
				ctx = context.Background()
			case "foreign":
				ctx = cortex.WithScope(ctx, cortex.Scope{Levels: []cortex.Level{{Key: "org", Value: "foreign"}}})
			case "cancelled":
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			provider := readFunc(func(context.Context, optimization.ArtifactRef) (optimization.Artifact, error) {
				t.Fatal("ineligible artifact reached provider")
				return optimization.Artifact{}, nil
			})
			reader, err := artifacts.NewRetainedReader(s, provider, 1024, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			got, err := reader.Get(ctx, req.Ref)
			if err == nil || len(got.Data) != 0 {
				t.Fatal("ineligible content returned")
			}
			if mode == "missing" && (!errors.Is(err, optimization.ErrNotFound) || !errors.Is(err, optimization.ErrIncompleteEvidence)) {
				t.Fatalf("missing classification: %v", err)
			}
		})
	}
}

func TestRetainedReaderRequiresProviderAndBounds(t *testing.T) {
	s, _, _ := fixture(t)
	provider := readFunc(func(context.Context, optimization.ArtifactRef) (optimization.Artifact, error) {
		return optimization.Artifact{}, nil
	})
	for _, test := range []struct {
		metadata artifacts.RetentionReader
		provider optimization.ArtifactReader
		limit    int64
		now      func() time.Time
	}{
		{nil, provider, 1, time.Now}, {s, nil, 1, time.Now}, {s, provider, 0, time.Now}, {s, provider, 1, nil},
	} {
		if _, err := artifacts.NewRetainedReader(test.metadata, test.provider, test.limit, test.now); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatal(err)
		}
	}
}
