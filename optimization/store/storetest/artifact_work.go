package storetest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
)

type artifactWorkReader interface {
	ListArtifactWork(context.Context, time.Time, optimization.PageQuery) (optimization.Page[optimization.ArtifactRetention], error)
}

func artifactWork(t *testing.T, factory Factory) {
	t.Helper()
	t.Run("ArtifactWorkPagesSurviveDeletion", func(t *testing.T) {
		now := time.Now().UTC()
		s := factory(t, func() time.Time { return now })
		reader, ok := s.(artifactWorkReader)
		if !ok {
			t.Fatal("store cannot discover artifact recovery work")
		}
		bundle := Fixture(t, "artifact-work")
		ctx := scoped(bundle.Experiment.Spec.Scope)
		lease := validationExperiment(ctx, t, s, bundle)
		refs := map[string]optimization.ArtifactRef{}
		for _, kind := range []string{"pending", "expired", "later", "deleted"} {
			registration := artifactRegistration(bundle, lease, now)
			registration.Ref.Kind = kind
			if kind == "expired" {
				registration.ExpiresAt = now.Add(time.Minute)
			}
			registerArtifact(ctx, t, s, registration)
			refs[kind] = registration.Ref
			if kind == "pending" || kind == "deleted" {
				request := optimization.ArtifactDeletion{Ref: registration.Ref, RequestID: kind, Actor: registration.Actor, Reason: "cleanup"}
				if err := s.Transact(ctx, func(tx store.Tx) error {
					if beginErr := tx.BeginArtifactDeletion(request); beginErr != nil {
						return beginErr
					}
					if kind == "deleted" {
						return tx.CompleteArtifactDeletion(request.Ref, request.RequestID)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		now = now.Add(time.Minute)
		cutoff := now
		query := optimization.PageQuery{Limit: 1}
		first, err := reader.ListArtifactWork(ctx, cutoff, query)
		if err != nil || len(first.Items) != 1 || first.NextCursor == "" {
			t.Fatalf("first page: %+v %v", first, err)
		}
		seen := map[string]bool{}
		for {
			page, listErr := reader.ListArtifactWork(ctx, cutoff, query)
			if listErr != nil {
				t.Fatal(listErr)
			}
			for _, item := range page.Items {
				kind := item.Ref.Kind
				if seen[kind] || (kind != "pending" && kind != "expired") {
					t.Fatalf("unexpected or repeated work: %s", kind)
				}
				seen[kind] = true
				request := optimization.ArtifactDeletion{Ref: item.Ref, RequestID: "expire", Actor: bundle.Experiment.Spec.Requester, Reason: "retention elapsed"}
				if item.Deletion != nil {
					request = *item.Deletion
				}
				if err = s.Transact(ctx, func(tx store.Tx) error {
					if beginErr := tx.BeginArtifactDeletion(request); beginErr != nil {
						return beginErr
					}
					return tx.CompleteArtifactDeletion(request.Ref, request.RequestID)
				}); err != nil {
					t.Fatal(err)
				}
			}
			if page.NextCursor == "" {
				break
			}
			query.Cursor = page.NextCursor
		}
		if len(seen) != 2 {
			t.Fatalf("skipped work after deletion: %v", seen)
		}
		first.Items[0].Ref.Scope.Levels[0].Value = "mutated"
		if _, err = s.GetArtifact(ctx, refs["pending"]); err != nil {
			t.Fatalf("list aliased stored scope: %v", err)
		}
		query.Cursor = first.NextCursor
		if _, err = reader.ListArtifactWork(ctx, cutoff.Add(-time.Nanosecond), query); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("cursor accepted changed cutoff: %v", err)
		}
		foreign := scoped(Fixture(t, "other-project").Experiment.Spec.Scope)
		if _, err = reader.ListArtifactWork(foreign, cutoff, query); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("cursor crossed scope: %v", err)
		}
		if page, listErr := reader.ListArtifactWork(foreign, cutoff, optimization.PageQuery{}); listErr != nil || len(page.Items) != 0 {
			t.Fatalf("foreign work exposed: %+v %v", page, listErr)
		}
		if _, err = reader.ListArtifactWork(context.Background(), cutoff, optimization.PageQuery{}); !errors.Is(err, cortex.ErrNoScope) {
			t.Fatalf("unscoped scan: %v", err)
		}
		for _, invalid := range []time.Time{{}, cutoff.Add(time.Second)} {
			if _, err = reader.ListArtifactWork(ctx, invalid, optimization.PageQuery{}); !errors.Is(err, optimization.ErrInvalidSpec) {
				t.Fatalf("invalid cutoff: %v", err)
			}
		}
		for _, limit := range []int{-1, 501} {
			if _, err = reader.ListArtifactWork(ctx, cutoff, optimization.PageQuery{Limit: limit}); !errors.Is(err, optimization.ErrInvalidSpec) {
				t.Fatalf("invalid limit: %v", err)
			}
		}
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		if _, err = reader.ListArtifactWork(cancelled, cutoff, optimization.PageQuery{}); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled scan: %v", err)
		}
	})
}
