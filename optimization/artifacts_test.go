package optimization

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
)

func artifactFixture() Artifact {
	data := []byte("captured trace")
	return Artifact{
		Ref:  ArtifactRef{ExperimentID: id.New(PrefixExperiment), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "one"}}}, Digest: digestBytes(data), MediaType: "text/plain", Size: int64(len(data)), AccessPolicy: "restricted", Kind: "trace"},
		Data: data, CreatedAt: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC),
	}
}

func TestArtifactBindingIncludesScopeAndPolicy(t *testing.T) {
	a := artifactFixture()
	want, err := a.Ref.BindingDigest()
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*ArtifactRef){
		"scope":      func(r *ArtifactRef) { r.Scope.Levels[0].Value = "two" },
		"policy":     func(r *ArtifactRef) { r.AccessPolicy = "public" },
		"kind":       func(r *ArtifactRef) { r.Kind = "output" },
		"media":      func(r *ArtifactRef) { r.MediaType = "application/json" },
		"size":       func(r *ArtifactRef) { r.Size++ },
		"digest":     func(r *ArtifactRef) { r.Digest = strings.Repeat("b", 64) },
		"experiment": func(r *ArtifactRef) { r.ExperimentID = id.New(PrefixExperiment) },
	} {
		t.Run(name, func(t *testing.T) {
			r := a.Ref
			r.Scope.Levels = append([]cortex.Level(nil), a.Ref.Scope.Levels...)
			mutate(&r)
			got, bindingErr := r.BindingDigest()
			if bindingErr != nil || got == want {
				t.Fatalf("binding unchanged: %q %v", got, bindingErr)
			}
		})
	}
}

func TestArtifactRejectsInvalidContentAndMetadata(t *testing.T) {
	for name, mutate := range map[string]func(*Artifact){
		"bytes":             func(a *Artifact) { a.Data[0]++ },
		"length":            func(a *Artifact) { a.Ref.Size++ },
		"digest case":       func(a *Artifact) { a.Ref.Digest = strings.ToUpper(a.Ref.Digest) },
		"negative size":     func(a *Artifact) { a.Ref.Size = -1 },
		"scope":             func(a *Artifact) { a.Ref.Scope.Levels = nil },
		"policy whitespace": func(a *Artifact) { a.Ref.AccessPolicy = " restricted" },
		"kind control":      func(a *Artifact) { a.Ref.Kind = "trace\n" },
		"media":             func(a *Artifact) { a.Ref.MediaType = "not media" },
		"experiment":        func(a *Artifact) { a.Ref.ExperimentID = id.NewAgentID() },
		"creation":          func(a *Artifact) { a.CreatedAt = time.Time{} },
		"retention":         func(a *Artifact) { a.ExpiresAt = time.Time{} },
		"empty retention":   func(a *Artifact) { a.ExpiresAt = a.CreatedAt },
	} {
		t.Run(name, func(t *testing.T) {
			a := artifactFixture()
			mutate(&a)
			if err := a.Validate(); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("accepted invalid artifact: %v", err)
			}
		})
	}
	a := artifactFixture()
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	a.Data = nil
	a.Ref.Size = 0
	a.Ref.Digest = digestBytes(nil)
	if err := a.Validate(); err != nil {
		t.Fatalf("valid empty content refused: %v", err)
	}
}

type artifactReadFunc func(context.Context, ArtifactRef) (Artifact, error)

func (f artifactReadFunc) Get(ctx context.Context, ref ArtifactRef) (Artifact, error) {
	return f(ctx, ref)
}

func TestArtifactVerificationRejectsUnavailableEvidence(t *testing.T) {
	for _, name := range []string{"missing", "denied", "corrupt", "policy", "experiment", "expired", "future", "oversized", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			a := artifactFixture()
			ctx, cancel := context.WithCancel(cortex.WithScope(context.Background(), a.Ref.Scope))
			defer cancel()
			now := a.CreatedAt.Add(time.Hour)
			cause := ErrNotFound
			reader := artifactReadFunc(func(context.Context, ArtifactRef) (Artifact, error) {
				switch name {
				case "missing":
					return Artifact{}, cause
				case "denied":
					cause = ErrPolicyDenied
					return Artifact{}, cause
				case "corrupt":
					a.Data[0]++
				case "policy":
					a.Ref.AccessPolicy = "public"
				case "experiment":
					a.Ref.ExperimentID = id.New(PrefixExperiment)
				case "expired":
					now = a.ExpiresAt
				case "future":
					now = a.CreatedAt.Add(-time.Second)
				case "oversized":
					a.Data = make([]byte, 1025)
				case "cancelled":
					cancel()
				}
				return a, nil
			})
			verifier, err := NewArtifactVerifier(reader, 1024, func() time.Time { return now })
			if err != nil {
				t.Fatal(err)
			}
			got, err := verifier.Verify(ctx, a.Ref.ExperimentID, a.Ref)
			if err == nil || len(got.Data) != 0 {
				t.Fatalf("unavailable evidence returned: %+v %v", got, err)
			}
			if name == "cancelled" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				return
			}
			if !errors.Is(err, ErrIncompleteEvidence) {
				t.Fatalf("missing incomplete classification: %v", err)
			}
			if (name == "missing" || name == "denied") && !errors.Is(err, cause) {
				t.Fatalf("lost cause: %v", err)
			}
		})
	}
}

func TestArtifactVerifierChecksBeforeReadingAndDetaches(t *testing.T) {
	a := artifactFixture()
	calls := 0
	reader := artifactReadFunc(func(ctx context.Context, ref ArtifactRef) (Artifact, error) {
		calls++
		ref.Scope.Levels[0].Value = "mutated"
		scope := cortex.ScopeFromContext(ctx)
		scope.Levels[0].Value = "mutated context"
		return a, nil
	})
	v, err := NewArtifactVerifier(reader, 1024, func() time.Time { return a.CreatedAt.Add(time.Hour) })
	if err != nil {
		t.Fatal(err)
	}
	ctx := cortex.WithScope(context.Background(), a.Ref.Scope)
	got, err := v.Verify(ctx, a.Ref.ExperimentID, a.Ref)
	if err != nil {
		t.Fatal(err)
	}
	got.Data[0]++
	got.Ref.Scope.Levels[0].Value = "changed result"
	if a.Ref.Scope.Levels[0].Value != "one" || string(a.Data) != "captured trace" {
		t.Fatal("caller or provider memory aliased")
	}
	for _, badCtx := range []context.Context{context.Background(), cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "two"}}})} {
		if _, err = v.Verify(badCtx, a.Ref.ExperimentID, a.Ref); err == nil {
			t.Fatal("invalid context reached provider")
		}
	}
	ref := a.Ref
	ref.Size = 1025
	if _, err = v.Verify(ctx, a.Ref.ExperimentID, ref); err == nil {
		t.Fatal("oversized ref reached provider")
	}
	if calls != 1 {
		t.Fatalf("provider called %d times", calls)
	}
}

func TestArtifactVerifierRequiresConfiguredBounds(t *testing.T) {
	reader := artifactReadFunc(func(context.Context, ArtifactRef) (Artifact, error) {
		t.Fatal("invalid verifier invoked reader")
		return Artifact{}, nil
	})
	for _, test := range []struct {
		reader ArtifactReader
		limit  int64
		clock  func() time.Time
	}{{nil, 1, time.Now}, {reader, 0, time.Now}, {reader, -1, time.Now}, {reader, 1, nil}} {
		if _, err := NewArtifactVerifier(test.reader, test.limit, test.clock); !errors.Is(err, ErrInvalidSpec) {
			t.Fatalf("invalid configuration accepted: %v", err)
		}
	}
}

func TestArtifactVerifierPreflightAndClockCancellation(t *testing.T) {
	a := artifactFixture()
	ctx, cancel := context.WithCancel(cortex.WithScope(context.Background(), a.Ref.Scope))
	defer cancel()
	calls := 0
	reader := artifactReadFunc(func(context.Context, ArtifactRef) (Artifact, error) { calls++; return a, nil })
	v, err := NewArtifactVerifier(reader, 1024, func() time.Time { cancel(); return a.CreatedAt })
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.Verify(ctx, id.NewAgentID(), a.Ref); !errors.Is(err, ErrInvalidSpec) {
		t.Fatal(err)
	}
	ref := a.Ref
	ref.Digest = "invalid"
	if _, err = v.Verify(ctx, a.Ref.ExperimentID, ref); !errors.Is(err, ErrInvalidSpec) {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("invalid input reached reader")
	}
	if got, verifyErr := v.Verify(ctx, a.Ref.ExperimentID, a.Ref); !errors.Is(verifyErr, context.Canceled) || len(got.Data) != 0 {
		t.Fatalf("late cancellation ignored: %v", verifyErr)
	}
	if _, err = v.Verify(ctx, a.Ref.ExperimentID, a.Ref); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("cancelled request reached reader")
	}
}

func TestExecutionUsesProtectedArtifactValidation(t *testing.T) {
	a := artifactFixture()
	end := a.CreatedAt.Add(time.Second)
	record := ExecutionRecord{
		RunID: id.NewAgentRunID(), PreparedDigest: a.Ref.Digest, InputDigest: a.Ref.Digest,
		State: run.StateCompleted, StartedAt: a.CreatedAt, CompletedAt: end, Duration: time.Second,
		Outcome:     run.Outcome{StopReason: run.StopFinalAnswer, CompletedAt: end},
		Environment: EnvironmentReceipt{ID: "receipt", Provider: "fixture", EnvironmentID: "isolated", Scope: a.Ref.Scope, AttemptID: id.New(PrefixAttempt), Snapshot: ContentRef{Ref: "snapshot", Revision: "1", Digest: a.Ref.Digest}, Fence: Fence{Owner: "worker", Generation: 1}},
		Artifacts:   []ArtifactRef{a.Ref},
	}
	if _, err := SealExecution(record); err != nil {
		t.Fatal(err)
	}
	record.Artifacts[0].AccessPolicy = " restricted"
	if _, err := SealExecution(record); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("execution accepted ambiguous policy: %v", err)
	}
	record.Artifacts[0] = a.Ref
	record.Artifacts[0].Scope = cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "two"}}}
	if _, err := SealExecution(record); !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("execution accepted different artifact scope: %v", err)
	}
}
