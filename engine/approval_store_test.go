package engine

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/agent"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
)

// newApprovalStore opens a migrated SQLite store with one agent in it,
// which is the smallest real backend a run can execute against. The
// engine doubles elsewhere in this package are enough to judge ordering
// and events; they are not enough to say a REST caller would see the
// checkpoint, because they are not what a REST caller reads.
func newApprovalStore(ctx context.Context, t *testing.T) *sqlitestore.Store {
	t.Helper()
	drv := sqlitedriver.New()
	if err := drv.Open(ctx, filepath.Join(t.TempDir(), "cortex_approval.db")); err != nil {
		t.Fatalf("open sqlite driver: %v", err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatalf("grove open: %v", err)
	}
	s := sqlitestore.New(db)
	if err := s.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if err := s.Create(ctx, &agent.Config{ID: id.NewAgentID(), Name: "assistant", MaxSteps: 2}); err != nil {
		t.Fatalf("create agent: %v", err)
	}
	return s
}

// TestApprovalCheckpoint_IsVisibleThroughTheCheckpointAPI runs the whole
// path against a real store and reads it back through the exact engine
// methods the REST handlers call: ListPendingCheckpoints is
// GET /v1/checkpoints, and ResolveCheckpoint is
// POST /v1/checkpoints/:id/resolve.
//
// Worth doing rather than assuming. The checkpoint surface has had a
// store, endpoints and hooks since v1.x with nothing writing to it, so
// "the endpoint exists" and "the endpoint returns what the loop wrote"
// were separate claims until this release.
func TestApprovalCheckpoint_IsVisibleThroughTheCheckpointAPI(t *testing.T) {
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := newApprovalStore(ctx, t)

	var ran int32
	def := gatedTool()
	e, err := New(
		WithStore(st),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{
			{ID: "call-1", Name: def.Name, Arguments: `{"target":"everything"}`},
		}}),
		WithTool(def, func(_ context.Context, _ cortex.Invocation) (string, error) {
			atomic.AddInt32(&ran, 1)
			return "deleted", nil
		}),
		WithToolAuthorizer(escalatingAuthorizer{tool: def.Name}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	paused, err := e.RunAgent(ctx, "assistant", "clean up", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if paused.State != run.StatePaused {
		t.Fatalf("run state = %q, want %q", paused.State, run.StatePaused)
	}

	// What GET /v1/checkpoints returns.
	pending, err := e.ListPendingCheckpoints(ctx, &checkpoint.ListFilter{Limit: 20})
	if err != nil {
		t.Fatalf("ListPendingCheckpoints: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("the checkpoint list returned %d rows, want the one the run just wrote", len(pending))
	}
	if pending[0].RunID != paused.ID {
		t.Errorf("listed checkpoint RunID = %q, want the paused run %q", pending[0].RunID, paused.ID)
	}
	if !strings.Contains(pending[0].Reason, def.Name) {
		t.Errorf("listed checkpoint reason = %q, want it to name the tool", pending[0].Reason)
	}
	if n, countErr := e.CountPendingCheckpoints(ctx, nil); countErr != nil || n != 1 {
		t.Errorf("CountPendingCheckpoints = (%d, %v), want (1, nil)", n, countErr)
	}
	if _, getErr := e.GetCheckpoint(ctx, pending[0].ID); getErr != nil {
		t.Errorf("GetCheckpoint on the row the loop wrote: %v", getErr)
	}

	// What POST /v1/checkpoints/:id/resolve does.
	if resolveErr := e.ResolveCheckpoint(ctx, pending[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "operator"}); resolveErr != nil {
		t.Fatalf("ResolveCheckpoint: %v", resolveErr)
	}

	if got := atomic.LoadInt32(&ran); got != 1 {
		t.Errorf("the approved tool ran %d times, want 1", got)
	}
	resumed, err := st.GetRun(ctx, paused.ID)
	if err != nil {
		t.Fatalf("reload the run: %v", err)
	}
	if resumed.State != run.StateCompleted {
		t.Fatalf("run state = %q, want %q", resumed.State, run.StateCompleted)
	}

	after, err := e.ListPendingCheckpoints(ctx, nil)
	if err != nil {
		t.Fatalf("ListPendingCheckpoints after the decision: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("the decided checkpoint is still listed as pending: %+v", after)
	}
}

// TestApprovalCheckpoint_RejectionIsVisibleThroughTheCheckpointAPI is the
// same round trip for the other answer, and it is the one that proves the
// reason a REST caller sends actually reaches the run.
func TestApprovalCheckpoint_RejectionIsVisibleThroughTheCheckpointAPI(t *testing.T) {
	const reason = "not on a Friday"
	ctx := cortex.WithScope(context.Background(), approvalScope())
	st := newApprovalStore(ctx, t)

	var ran int32
	def := gatedTool()
	e, err := New(
		WithStore(st),
		WithLLM(&scriptedLLM{toolCalls: []llm.ToolCall{{ID: "call-1", Name: def.Name, Arguments: "{}"}}}),
		WithTool(def, func(_ context.Context, _ cortex.Invocation) (string, error) {
			atomic.AddInt32(&ran, 1)
			return "deleted", nil
		}),
		WithToolAuthorizer(escalatingAuthorizer{tool: def.Name}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	paused, err := e.RunAgent(ctx, "assistant", "clean up", nil)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	pending, err := e.ListPendingCheckpoints(ctx, nil)
	if err != nil || len(pending) != 1 {
		t.Fatalf("ListPendingCheckpoints = (%d rows, %v), want 1 row", len(pending), err)
	}

	if resolveErr := e.ResolveCheckpoint(ctx, pending[0].ID, checkpoint.Decision{DecidedBy: "operator", Reason: reason}); resolveErr != nil {
		t.Fatalf("ResolveCheckpoint: %v", resolveErr)
	}

	rejected, err := st.GetRun(ctx, paused.ID)
	if err != nil {
		t.Fatalf("reload the run: %v", err)
	}
	if rejected.State != run.StateFailed {
		t.Fatalf("run state = %q, want %q", rejected.State, run.StateFailed)
	}
	if !strings.Contains(rejected.Error, reason) {
		t.Errorf("run error = %q, want the decision's reason %q", rejected.Error, reason)
	}
	if got := atomic.LoadInt32(&ran); got != 0 {
		t.Errorf("the rejected tool ran %d times, want 0", got)
	}
	if _, err := st.GetSuspension(ctx, paused.ID); err == nil {
		t.Error("the suspension survived a rejected run; it says paused about a failed run")
	}
}
