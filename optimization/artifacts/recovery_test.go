package artifacts_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/artifacts"
	"github.com/xraph/cortex/optimization/store/memory"
)

func TestRecoveryScansExpiredAndPendingWithOriginalAuthority(t *testing.T) {
	now := time.Now().UTC()
	s, err := memory.New(memory.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, pending := seed(t, s)
	_, expired := seed(t, s)
	if err = s.Transact(ctx, func(tx optimization.Transaction) error { return tx.BeginArtifactDeletion(pending) }); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Hour)
	actor := optimization.IdentityRef{Subject: "retention-worker"}
	failed := errors.New("provider unavailable")
	calls := 0
	provider := deleteFunc(func(_ context.Context, req optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		calls++
		if req.Ref.ExperimentID == pending.Ref.ExperimentID {
			if req.Actor != pending.Actor || req.RequestID != pending.RequestID || req.Reason != pending.Reason {
				t.Fatal("recovery replaced saved authority")
			}
			return artifacts.DeletionReceipt{}, failed
		}
		if req.Ref.ExperimentID != expired.Ref.ExperimentID || req.Actor != actor {
			t.Fatal("expiry used wrong identity")
		}
		return receipt(req), nil
	})
	runner, err := artifacts.NewRecovery(s, service(t, s, provider, nil, nil), actor, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	query := optimization.PageQuery{Limit: 1}
	total, failures := 0, 0
	for {
		result, runErr := runner.RunPage(ctx, now, query)
		if runErr != nil {
			t.Fatal(runErr)
		}
		for _, outcome := range result.Outcomes {
			total++
			if outcome.Err != nil {
				failures++
				if !errors.Is(outcome.Err, failed) || outcome.Ref.ExperimentID != pending.Ref.ExperimentID {
					t.Fatalf("wrong failure: %+v", outcome)
				}
			}
		}
		if result.NextCursor == "" {
			break
		}
		query.Cursor = result.NextCursor
	}
	if calls != 2 || total != 2 || failures != 1 {
		t.Fatalf("calls=%d outcomes=%d failures=%d", calls, total, failures)
	}
	got, err := s.GetArtifact(ctx, expired.Ref)
	if err != nil || got.DeletedAt.IsZero() || got.Deletion.Actor != actor {
		t.Fatalf("expiry not deleted: %+v %v", got, err)
	}
	got, err = s.GetArtifact(ctx, pending.Ref)
	if err != nil || !got.DeletedAt.IsZero() || got.Deletion.RequestID != pending.RequestID {
		t.Fatalf("failed work lost: %+v %v", got, err)
	}
	// A new runner and full sweep retry only the pending object.
	runner, err = artifacts.NewRecovery(s, service(t, s, func(_ context.Context, req optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		return receipt(req), nil
	}, nil, nil), actor, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.RunPage(ctx, now, optimization.PageQuery{})
	if err != nil || len(result.Outcomes) != 1 || result.Outcomes[0].Err != nil {
		t.Fatalf("retry: %+v %v", result, err)
	}
	got, err = s.GetArtifact(ctx, pending.Ref)
	if err != nil || got.DeletedAt.IsZero() {
		t.Fatalf("pending recovery not persisted: %+v %v", got, err)
	}
}

func TestRecoveryTimeoutLeavesWorkPendingAndContinues(t *testing.T) {
	s, ctx, first := fixture(t)
	_, second := seed(t, s)
	for _, req := range []optimization.ArtifactDeletion{first, second} {
		if err := s.Transact(ctx, func(tx optimization.Transaction) error { return tx.BeginArtifactDeletion(req) }); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	provider := deleteFunc(func(ctx context.Context, req optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		calls++
		if calls == 1 {
			<-ctx.Done()
			return artifacts.DeletionReceipt{}, ctx.Err()
		}
		return receipt(req), nil
	})
	runner, err := artifacts.NewRecovery(s, service(t, s, provider, nil, nil), first.Actor, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.RunPage(ctx, time.Now(), optimization.PageQuery{Limit: 2})
	if err != nil || calls != 2 || len(result.Outcomes) != 2 || !errors.Is(result.Outcomes[0].Err, context.DeadlineExceeded) || result.Outcomes[1].Err != nil {
		t.Fatalf("timeout stopped page: %+v calls=%d err=%v", result, calls, err)
	}
	got, err := s.GetArtifact(ctx, result.Outcomes[0].Ref)
	if err != nil || !got.DeletedAt.IsZero() {
		t.Fatalf("timed out object completed: %+v %v", got, err)
	}
}

func TestRecoveryCancellationDoesNotAdvanceCursor(t *testing.T) {
	s, ctx, req := fixture(t)
	for range 2 {
		_, other := seed(t, s)
		if err := s.Transact(ctx, func(tx optimization.Transaction) error { return tx.BeginArtifactDeletion(other) }); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Transact(ctx, func(tx optimization.Transaction) error { return tx.BeginArtifactDeletion(req) }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	calls := 0
	provider := deleteFunc(func(_ context.Context, req optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		calls++
		cancel()
		return receipt(req), nil
	})
	runner, err := artifacts.NewRecovery(s, service(t, s, provider, nil, nil), req.Actor, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.RunPage(ctx, time.Now(), optimization.PageQuery{Limit: 1})
	if !errors.Is(err, context.Canceled) || result.NextCursor != "" || calls != 1 {
		t.Fatalf("cancelled page advanced: %+v calls=%d err=%v", result, calls, err)
	}
}

func TestRecoveryRejectsInvalidConfigurationAndUnscopedScan(t *testing.T) {
	s, ctx, req := fixture(t)
	calls := 0
	deletion := service(t, s, func(_ context.Context, req optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		calls++
		return receipt(req), nil
	}, nil, nil)
	for _, timeout := range []time.Duration{-time.Second, time.Hour} {
		if _, err := artifacts.NewRecovery(s, deletion, req.Actor, timeout); !errors.Is(err, optimization.ErrInvalidSpec) {
			t.Fatalf("timeout accepted: %v", err)
		}
	}
	if _, err := artifacts.NewRecovery(s, deletion, optimization.IdentityRef{}, 0); !errors.Is(err, optimization.ErrInvalidSpec) {
		t.Fatalf("missing actor accepted: %v", err)
	}
	if _, err := artifacts.NewRecovery(nil, deletion, req.Actor, 0); !errors.Is(err, optimization.ErrInvalidSpec) {
		t.Fatalf("missing metadata accepted: %v", err)
	}
	if _, err := artifacts.NewRecovery(s, nil, req.Actor, 0); !errors.Is(err, optimization.ErrInvalidSpec) {
		t.Fatalf("missing deletion service accepted: %v", err)
	}
	runner, err := artifacts.NewRecovery(s, deletion, req.Actor, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runner.RunPage(context.Background(), time.Now(), optimization.PageQuery{}); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatalf("unscoped recovery: %v", err)
	}
	if _, err = runner.RunPage(ctx, time.Now().Add(time.Hour), optimization.PageQuery{}); !errors.Is(err, optimization.ErrInvalidSpec) {
		t.Fatalf("future recovery: %v", err)
	}
	if calls != 0 {
		t.Fatal("invalid scan called provider")
	}
}

type recoveryMetadata struct {
	artifacts.RecoveryMetadata
	list func(context.Context, time.Time, optimization.PageQuery) (optimization.Page[optimization.ArtifactRetention], error)
}

func (m recoveryMetadata) ListArtifactWork(ctx context.Context, cutoff time.Time, query optimization.PageQuery) (optimization.Page[optimization.ArtifactRetention], error) {
	return m.list(ctx, cutoff, query)
}

func TestRecoveryCancellationDuringEmptyListIsAnError(t *testing.T) {
	s, ctx, req := fixture(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	metadata := recoveryMetadata{RecoveryMetadata: s, list: func(context.Context, time.Time, optimization.PageQuery) (optimization.Page[optimization.ArtifactRetention], error) {
		cancel()
		return optimization.Page[optimization.ArtifactRetention]{}, nil
	}}
	runner, err := artifacts.NewRecovery(metadata, service(t, s, func(context.Context, optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		t.Fatal("unexpected provider call")
		return artifacts.DeletionReceipt{}, nil
	}, nil, nil), req.Actor, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = runner.RunPage(ctx, time.Now(), optimization.PageQuery{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled discovery reported success: %v", err)
	}
}

func TestRecoveryRechecksDeletionCreatedAfterListing(t *testing.T) {
	now := time.Now().UTC()
	s, err := memory.New(memory.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx, req := seed(t, s)
	now = now.Add(2 * time.Hour)
	metadata := recoveryMetadata{RecoveryMetadata: s, list: func(ctx context.Context, cutoff time.Time, query optimization.PageQuery) (optimization.Page[optimization.ArtifactRetention], error) {
		page, listErr := s.ListArtifactWork(ctx, cutoff, query)
		if listErr != nil {
			return page, listErr
		}
		return page, s.Transact(ctx, func(tx optimization.Transaction) error { return tx.BeginArtifactDeletion(req) })
	}}
	provider := deleteFunc(func(_ context.Context, saved optimization.ArtifactDeletion) (artifacts.DeletionReceipt, error) {
		if saved.RequestID != req.RequestID || saved.Actor != req.Actor {
			t.Fatal("stale scan replaced concurrent request")
		}
		return receipt(saved), nil
	})
	runner, err := artifacts.NewRecovery(metadata, service(t, s, provider, nil, nil), optimization.IdentityRef{Subject: "retention-worker"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	result, err := runner.RunPage(ctx, now, optimization.PageQuery{})
	if err != nil || len(result.Outcomes) != 1 || result.Outcomes[0].Err != nil {
		t.Fatalf("concurrent request not recovered: %+v %v", result, err)
	}
	got, err := s.GetArtifact(ctx, req.Ref)
	if err != nil || got.DeletedAt.IsZero() {
		t.Fatalf("concurrent deletion incomplete: %+v %v", got, err)
	}
}
