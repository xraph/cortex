package scopespy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/suspension"
)

// The spy's progress methods follow the real stores' fence closely enough
// that an engine test against it cannot save progress a real store would
// refuse.
func TestSpy_ProgressFollowsStoreRules(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}})
	other := cortex.WithScope(context.Background(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_y"}}})
	spy := New()
	r := &run.Run{ID: id.NewAgentRunID(), State: run.StateRunning, Lease: &run.Lease{Owner: "w1", ExpiresAt: time.Now().Add(time.Minute)}}
	if err := spy.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	p := &suspension.Progress{RunID: r.ID, Owner: "w1", Cont: suspension.Continuation{Prepared: []byte(`{"a":1}`)}}
	if err := spy.SaveProgress(ctx, p); err != nil {
		t.Fatalf("SaveProgress by the lease owner: %v", err)
	}
	if p.UpdatedAt.IsZero() || p.Scope.Canonical() != cortex.ScopeFromContext(ctx).Canonical() {
		t.Fatalf("SaveProgress left UpdatedAt %v and scope %q, want both set as the real stores set them", p.UpdatedAt, p.Scope.Canonical())
	}
	for _, owner := range []string{"w2", ""} {
		if err := spy.SaveProgress(ctx, &suspension.Progress{RunID: r.ID, Owner: owner}); !errors.Is(err, cortex.ErrRunLeaseLost) {
			t.Fatalf("SaveProgress by owner %q = %v, want ErrRunLeaseLost", owner, err)
		}
	}
	got, err := spy.GetProgress(ctx, r.ID)
	if err != nil || got.Owner != "w1" || string(got.Cont.Prepared) != `{"a":1}` || !got.UpdatedAt.Equal(p.UpdatedAt) {
		t.Fatalf("GetProgress = %+v, %v", got, err)
	}
	if _, err = spy.GetProgress(other, r.ID); !errors.Is(err, cortex.ErrProgressNotFound) {
		t.Fatalf("GetProgress from another scope = %v, want ErrProgressNotFound", err)
	}
	if err = spy.DeleteProgress(other, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = spy.GetProgress(ctx, r.ID); err != nil {
		t.Fatalf("DeleteProgress from another scope removed the snapshot: %v", err)
	}
	for range 2 {
		if err = spy.DeleteProgress(ctx, r.ID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = spy.GetProgress(ctx, r.ID); !errors.Is(err, cortex.ErrProgressNotFound) {
		t.Fatalf("GetProgress after delete = %v, want ErrProgressNotFound", err)
	}
	if _, err = spy.ClaimContinuation(ctx, r.ID, run.Lease{Owner: "w1"}); !errors.Is(err, cortex.ErrRunLeaseLost) {
		t.Fatalf("ClaimContinuation with a half lease = %v, want ErrRunLeaseLost", err)
	}
}
