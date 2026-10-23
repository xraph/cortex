package storetest

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

func testRunRevision(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx := ctxWithScope("run-revision")
	r := mustCreateRun(t, s, ctx)
	if r.Revision != 1 {
		t.Fatalf("initial revision = %d", r.Revision)
	}
	stale, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	r.Output = "current result"
	if err = s.UpdateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	if r.Revision != 2 {
		t.Fatalf("updated revision = %d", r.Revision)
	}
	stale.Output = "obsolete result"
	before := stale.UpdatedAt
	if err = s.UpdateRun(ctx, stale); !errors.Is(err, cortex.ErrRunConflict) {
		t.Fatal("stale writer overwrote current run")
	}
	if !stale.UpdatedAt.Equal(before) {
		t.Fatal("failed update changed caller timestamp")
	}
	got, err := s.GetRun(ctx, r.ID)
	if err != nil || got.Revision != 2 || got.Output != "current result" {
		t.Fatalf("current result lost: %+v, %v", got, err)
	}
	listed, err := s.ListRuns(ctx, &run.ListFilter{Exact: true})
	if err != nil || len(listed) != 1 || listed[0].Revision != got.Revision {
		t.Fatalf("list lost revision: %+v, %v", listed, err)
	}
}

func testRunRevisionClaims(t *testing.T, newStore func(*testing.T) store.Store) {
	for _, expiry := range []bool{false, true} {
		t.Run(fmt.Sprintf("expired=%v", expiry), func(t *testing.T) {
			s := newStore(t)
			ctx := ctxWithScope("claim-revision")
			var deadline *time.Time
			if expiry {
				deadline = pastDeadline()
			}
			r, _ := mustSuspendRun(t, s, ctx, deadline)
			changed := *r
			changed.Output = "updated while paused"
			if err := s.UpdateRun(ctx, &changed); err != nil {
				t.Fatal(err)
			}
			before := changed.Revision
			var claim *suspension.Suspension
			var err error
			if expiry {
				claim, err = s.ClaimExpiredSuspension(ctx, r.ID, time.Now())
			} else {
				claim, err = s.ClaimSuspension(ctx, r.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if claim.ClaimedRun == nil || claim.ClaimedRun.Revision != before+1 || claim.ClaimedRun.State != run.StateRunning || claim.ClaimedRun.ID != r.ID || claim.ClaimedRun.Output != changed.Output {
				t.Fatalf("invalid claim snapshot: %+v", claim.ClaimedRun)
			}
			current, err := s.GetRun(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if current.Revision != before+1 {
				t.Fatalf("claim kept revision %d, wanted %d", current.Revision, before+1)
			}
			if err = s.UpdateRun(ctx, r); !errors.Is(err, cortex.ErrRunConflict) {
				t.Fatalf("pre-claim write = %v", err)
			}
		})
	}
}

func testRunRevisionConcurrent(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx := ctxWithScope("concurrent-revision")
	r := mustCreateRun(t, s, ctx)
	const writers = 8
	start := make(chan struct{})
	results := make(chan error, writers)
	for i := range writers {
		copyRun := *r
		copyRun.Output = fmt.Sprint(i)
		go func() { <-start; results <- s.UpdateRun(ctx, &copyRun) }()
	}
	close(start)
	wins := 0
	for range writers {
		err := <-results
		if err == nil {
			wins++
		} else if !errors.Is(err, cortex.ErrRunConflict) {
			t.Errorf("unexpected loser: %v", err)
		}
	}
	if wins != 1 {
		t.Fatalf("got %d winning writers", wins)
	}
	current, err := s.GetRun(ctx, r.ID)
	if err != nil || current.Revision != r.Revision+1 {
		t.Fatalf("revision after race: %+v, %v", current, err)
	}
}

func testRunRevisionDenied(t *testing.T, newStore func(*testing.T) store.Store) {
	s := newStore(t)
	ctx := cortex.WithScope(context.Background(), scopeOf("revision", "project"))
	r := mustCreateRun(t, s, ctx)
	for _, revision := range []int64{-1, 0, math.MaxInt64} {
		copyRun := *r
		copyRun.Revision = revision
		if err := s.UpdateRun(ctx, &copyRun); !errors.Is(err, cortex.ErrRunConflict) {
			t.Fatalf("revision %d: %v", revision, err)
		}
		if copyRun.Revision != revision || !copyRun.UpdatedAt.Equal(r.UpdatedAt) {
			t.Fatal("failed update changed metadata")
		}
	}
	for _, scope := range []cortex.Scope{scopeOf("revision"), scopeOf("other", "project")} {
		if err := s.UpdateRun(cortex.WithScope(ctx, scope), r); !errors.Is(err, cortex.ErrRunNotFound) {
			t.Fatalf("denied scope: %v", err)
		}
	}
	missing := *r
	missing.ID = id.NewAgentRunID()
	if err := s.UpdateRun(ctx, &missing); !errors.Is(err, cortex.ErrRunNotFound) {
		t.Fatalf("missing run: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.UpdateRun(cancelled, r); err == nil {
		t.Fatal("cancelled write accepted")
	}
	current, err := s.GetRun(ctx, r.ID)
	if err != nil || current.Revision != 1 {
		t.Fatalf("denied writes changed revision: %+v, %v", current, err)
	}
}
