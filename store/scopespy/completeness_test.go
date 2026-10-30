package scopespy

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/checkpoint"
	"github.com/xraph/cortex/engine"
	"github.com/xraph/cortex/llm"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store"
	"github.com/xraph/cortex/suspension"
)

// overriddenMethods probes every method on the store.Store interface by
// calling it via reflection on a fresh Spy, with zero-value arguments and
// a scoped ctx wherever a ctx parameter is expected. Spy embeds
// store.Store as a nil interface and overrides only what it needs to, so
// dispatching a call to any method it hasn't explicitly overridden
// panics on the nil interface before the callee's body ever runs — that
// happens regardless of the arguments, since a nil interface has no
// concrete type to dispatch to. A method that returns cleanly and gets
// recorded therefore proves Spy overrides it; this is the same
// call-and-recover technique the old hand-maintained version of this
// test used, just run across the full interface instead of a fixed list.
func overriddenMethods(t *testing.T) map[string]bool {
	t.Helper()
	storeType := reflect.TypeOf((*store.Store)(nil)).Elem()
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	ctx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})

	overridden := make(map[string]bool, storeType.NumMethod())
	for i := 0; i < storeType.NumMethod(); i++ {
		name := storeType.Method(i).Name
		spy := New()
		mv := reflect.ValueOf(spy).MethodByName(name)
		if !mv.IsValid() {
			t.Fatalf("*Spy has no method %q despite store.Store requiring it", name)
		}

		mt := mv.Type()
		args := make([]reflect.Value, mt.NumIn())
		for j := range args {
			pt := mt.In(j)
			if pt == ctxType {
				args[j] = reflect.ValueOf(ctx)
				continue
			}
			args[j] = reflect.Zero(pt)
		}

		func() {
			defer func() { _ = recover() }() // not overridden: fell through to the embedded nil Store
			mv.Call(args)
		}()

		if spy.CallCount() == 1 {
			overridden[name] = true
		}
	}
	return overridden
}

// reachedMethods runs two real scenarios against fresh Spy instances and
// unions the store.Store method names either one actually invoked:
//
//   - RunAgent, with a tool-calling LLM double so CreateToolCall is
//     exercised too, alongside every method a plain single-step run
//     already reaches.
//   - RunAgent again, this time against an external tool, so the
//     suspend path (CreateSuspension) is exercised. A run that finishes
//     normally never writes a suspension, so without this scenario the
//     Spy override for it would look dead.
//   - StreamAgent, which runs its store work (LoadConversation,
//     CreateStep, SaveConversation, UpdateRun, ...) from a goroutine
//     inside streamReAct — a separate code path from runReAct that could
//     reach a method the synchronous run above never does, or reach the
//     same methods through call sites RunAgent alone wouldn't exercise.
//     Running only RunAgent here would make the equality check in
//     TestSpy_OverriddenMethodsMatchWhatTheReactLoopReaches fail on any
//     override Spy carries solely for the streaming path.
//
// escalatingAuthorizer answers every dispatch with ErrRequiresApproval,
// which is the authorizer's third answer: not a denial, a request for a
// human.
type escalatingAuthorizer struct{}

func (escalatingAuthorizer) Visible(_ context.Context, _ cortex.Subject, tools []llm.Tool) []llm.Tool {
	return tools
}

func (escalatingAuthorizer) Authorize(_ context.Context, _ cortex.Subject, _ llm.ToolCall) error {
	return cortex.ErrRequiresApproval
}

func reachedMethods(t *testing.T) map[string]bool {
	t.Helper()
	reached := make(map[string]bool)

	const toolName = "spy-completeness-tool"
	runSpy := New()
	runEngine, err := engine.New(
		engine.WithStore(runSpy),
		engine.WithLLM(ToolCallingLLM(toolName)),
		engine.WithTool(
			llm.Tool{Name: toolName, Description: "test-only tool for completeness coverage"},
			func(_ context.Context, _ cortex.Invocation) (string, error) { return "ok", nil },
		),
	)
	if err != nil {
		t.Fatalf("engine.New (RunAgent): %v", err)
	}
	runCtx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	if _, runErr := runEngine.RunAgent(runCtx, "assistant", "hello", nil); runErr != nil {
		t.Fatalf("RunAgent: %v", runErr)
	}
	for _, c := range runSpy.Calls() {
		reached[c.Method] = true
	}

	const externalToolName = "spy-completeness-external-tool"
	suspendSpy := New()
	// Leases off, so the resume makes the plain claim. An engine that
	// leases runs claims with the lease instead (ClaimSuspensionLeased),
	// which TestSpy_OverridesLeasedClaims probes.
	suspendEngine, err := engine.New(
		engine.WithStore(suspendSpy),
		engine.WithRunLease(0),
		engine.WithLLM(ToolCallingLLM(externalToolName)),
		engine.WithExternalTool(llm.Tool{
			Name:        externalToolName,
			Description: "test-only external tool for completeness coverage",
		}),
	)
	if err != nil {
		t.Fatalf("engine.New (suspending RunAgent): %v", err)
	}
	suspended, err := suspendEngine.RunAgent(runCtx, "assistant", "hello", nil)
	if err != nil {
		t.Fatalf("RunAgent (suspending): %v", err)
	}
	if suspended.State != run.StatePaused {
		t.Fatalf("the suspend scenario left the run in %q, so it never reached CreateSuspension", suspended.State)
	}

	// The suspended run is the fixture for a resume, on the same engine
	// and the same spy so the resume reads exactly the suspension the run
	// just wrote. Resume reaches the claim, the run and agent reads, the
	// step lookup and the delete: six methods no run that finishes in one
	// pass ever touches.
	susps := suspendSpy.Suspensions()
	if len(susps) != 1 || len(susps[0].Pending) != 1 {
		t.Fatalf("the suspend scenario wrote %d suspensions; the resume scenario needs exactly 1 with 1 pending call", len(susps))
	}
	resumed, err := suspendEngine.Resume(runCtx, suspended.ID, engine.ResumeInput{
		ToolResults: []engine.ToolResult{{ToolCallID: susps[0].Pending[0].ID, Content: "the caller ran it"}},
	})
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if resumed.State != run.StateCompleted {
		t.Fatalf("the resume scenario left the run in %q, so it never ran the loop to completion", resumed.State)
	}
	for _, c := range suspendSpy.Calls() {
		reached[c.Method] = true
	}

	// A run whose authorizer escalates instead of denying. This is the
	// only scenario that reaches CreateCheckpoint: an approval is the
	// one suspension reason that opens a checkpoint, so without it the
	// override would look dead.
	const approvalToolName = "spy-completeness-approval-tool"
	approvalSpy := New()
	approvalEngine, err := engine.New(
		engine.WithStore(approvalSpy),
		engine.WithLLM(ToolCallingLLM(approvalToolName)),
		engine.WithTool(
			llm.Tool{Name: approvalToolName, Description: "test-only tool for approval coverage"},
			func(_ context.Context, _ cortex.Invocation) (string, error) { return "ok", nil },
		),
		engine.WithToolAuthorizer(escalatingAuthorizer{}), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()),
	)
	if err != nil {
		t.Fatalf("engine.New (approval): %v", err)
	}
	approved, err := approvalEngine.RunAgent(runCtx, "assistant", "do the risky thing", nil)
	if err != nil {
		t.Fatalf("RunAgent (approval): %v", err)
	}
	if approved.State != run.StatePaused {
		t.Fatalf("the approval scenario left the run in %q, so it never reached CreateCheckpoint", approved.State)
	}
	// Approving it is the only route to GetCheckpoint and Resolve, and it
	// carries the run through to completion on the same spy.
	approvalCps := approvalSpy.Checkpoints()
	if len(approvalCps) != 1 {
		t.Fatalf("the approval scenario wrote %d checkpoints; resolving needs exactly 1", len(approvalCps))
	}
	if resolveErr := approvalEngine.ResolveCheckpoint(runCtx, approvalCps[0].ID, checkpoint.Decision{Approved: true, DecidedBy: "completeness"}); resolveErr != nil {
		t.Fatalf("ResolveCheckpoint: %v", resolveErr)
	}
	resolvedRun, err := approvalSpy.GetRun(runCtx, approved.ID)
	if err != nil {
		t.Fatalf("reload the approved run: %v", err)
	}
	if resolvedRun.State != run.StateCompleted {
		t.Fatalf("the approval scenario left the run in %q, so the approved resume never ran the loop", resolvedRun.State)
	}
	for _, c := range approvalSpy.Calls() {
		reached[c.Method] = true
	}

	// A paused run an operator cancels. Cancel claims the suspension the
	// way every other writer to a paused run does, and then clears what
	// the pause left behind, which is the only route to ListPending.
	const cancelToolName = "spy-completeness-cancel-tool"
	cancelSpy := New()
	cancelEngine, err := engine.New(
		engine.WithStore(cancelSpy),
		engine.WithLLM(ToolCallingLLM(cancelToolName)),
		engine.WithTool(
			llm.Tool{Name: cancelToolName, Description: "test-only tool for cancel coverage"},
			func(_ context.Context, _ cortex.Invocation) (string, error) { return "ok", nil },
		),
		engine.WithToolAuthorizer(escalatingAuthorizer{}), engine.WithPrincipalCodec(cortex.JSONPrincipalCodec[string]()),
	)
	if err != nil {
		t.Fatalf("engine.New (cancel): %v", err)
	}
	cancelled, err := cancelEngine.RunAgent(runCtx, "assistant", "do the risky thing", nil)
	if err != nil {
		t.Fatalf("RunAgent (cancel): %v", err)
	}
	if cancelled.State != run.StatePaused {
		t.Fatalf("the cancel scenario left the run in %q, so there was no paused run to cancel", cancelled.State)
	}
	if cancelErr := cancelEngine.CancelRun(runCtx, cancelled.ID); cancelErr != nil {
		t.Fatalf("CancelRun: %v", cancelErr)
	}
	for _, c := range cancelSpy.Calls() {
		reached[c.Method] = true
	}

	streamSpy := New()
	streamEngine, err := engine.New(
		engine.WithStore(streamSpy),
		engine.WithLLM(StaticStreamLLM("done")),
	)
	if err != nil {
		t.Fatalf("engine.New (StreamAgent): %v", err)
	}
	streamCtx := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	events := make(chan engine.StreamEvent, 64)
	if err := streamEngine.StreamAgent(streamCtx, "assistant", "hello", nil, events); err != nil {
		t.Fatalf("StreamAgent: %v", err)
	}
	var drained int
	for range events {
		// Drain to completion: streamReAct closes events once its
		// goroutine finishes all store work, so this blocks until every
		// call below has already happened.
		drained++
	}
	if drained == 0 {
		t.Fatal("no stream events received; StreamAgent may not have run")
	}
	for _, c := range streamSpy.Calls() {
		reached[c.Method] = true
	}

	return reached
}

// TestSpy_OverriddenMethodsMatchWhatTheReactLoopReaches replaces a
// hand-maintained inventory of "the methods the react loop reaches" with
// two independently derived sets compared against each other, closing a
// gap the old fixed-list version had: a fixed list only fails on a
// rename or a dropped Spy override, never on a genuinely new call site
// the loop starts making, because such a call would simply panic
// wherever the loop next runs against Spy — a real failure, but not one
// this test attributed to a completeness gap.
//
// overriddenMethods proves what Spy actually overrides; reachedMethods
// proves what a real RunAgent run AND a real StreamAgent run actually
// touch, unioned (a tool-calling LLM double on the RunAgent side covers
// CreateToolCall, the one call site a plain non-tool run never reaches;
// StreamAgent covers the goroutine-based streamReAct path, which is a
// separate code path from the synchronous runReAct one). This test
// asserts the two sets are equal: if either loop starts calling
// something Spy doesn't override, or Spy gains an override nothing
// calls anymore, the mismatch says which side moved instead of a bare
// nil-pointer panic somewhere else.
func TestSpy_OverriddenMethodsMatchWhatTheReactLoopReaches(t *testing.T) {
	overridden := overriddenMethods(t)
	reached := reachedMethods(t)

	if len(reached) == 0 {
		t.Fatal("reachedMethods recorded no calls; this test proves nothing")
	}

	// The optional capabilities the engine asserts by type are probed by
	// TestSpy_OverridesContinuationStore and TestSpy_OverridesProgressStore.
	// A scenario may reach them (an approval continues through
	// ClaimContinuation), but nothing here has to reach every one of them.
	capabilities := make(map[string]bool)
	for _, capability := range []reflect.Type{
		reflect.TypeOf((*run.ContinuationStore)(nil)).Elem(),
		reflect.TypeOf((*suspension.ProgressStore)(nil)).Elem(),
		reflect.TypeOf((*checkpoint.SweepStore)(nil)).Elem(),
		reflect.TypeOf((*suspension.LeasedClaimStore)(nil)).Elem(),
	} {
		for i := 0; i < capability.NumMethod(); i++ {
			capabilities[capability.Method(i).Name] = true
		}
	}

	for name := range reached {
		if !overridden[name] && !capabilities[name] {
			t.Errorf("RunAgent or StreamAgent called %q, but Spy doesn't override it (overriddenMethods disagrees with reachedMethods — investigate before trusting either)", name)
		}
	}
	for name := range overridden {
		if !reached[name] {
			t.Errorf("Spy overrides %q, but neither RunAgent nor StreamAgent called it in this test — either the override is dead, or reachedMethods needs a scenario that reaches it", name)
		}
	}
}

// TestSpy_OverridesContinuationStore probes every run.ContinuationStore
// method the way overriddenMethods probes store.Store. The capability is
// optional, so it is not part of store.Store and the test above cannot
// see it; an engine that finds the spy implements it will call these
// methods, and each one has to record its call rather than panic on the
// embedded nil Store. The two AcrossScopes listings are the sweeper's and
// run without a scope, so they are probed with none.
func TestSpy_OverridesContinuationStore(t *testing.T) {
	probeCapability(t, reflect.TypeOf((*run.ContinuationStore)(nil)).Elem())
}

// TestSpy_OverridesLeasedClaims does the same for suspension.LeasedClaimStore,
// the claims that take the run's lease with the pause.
func TestSpy_OverridesLeasedClaims(t *testing.T) {
	probeCapability(t, reflect.TypeOf((*suspension.LeasedClaimStore)(nil)).Elem())
}

// TestSpy_OverridesCheckpointSweep does the same for checkpoint.SweepStore,
// the sweeper's listing of pending checkpoints.
func TestSpy_OverridesCheckpointSweep(t *testing.T) {
	probeCapability(t, reflect.TypeOf((*checkpoint.SweepStore)(nil)).Elem())
}

// TestSpy_OverridesProgressStore does the same for suspension.ProgressStore,
// the other optional capability the engine asserts by type.
func TestSpy_OverridesProgressStore(t *testing.T) {
	probeCapability(t, reflect.TypeOf((*suspension.ProgressStore)(nil)).Elem())
}

// probeCapability calls every method of capability on a fresh Spy with zero
// arguments and checks each one records exactly its own call, with a scope
// unless it is an AcrossScopes listing.
func probeCapability(t *testing.T, capability reflect.Type) {
	t.Helper()
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	scoped := cortex.WithScope(context.Background(), cortex.Scope{
		Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}},
	})
	for i := 0; i < capability.NumMethod(); i++ {
		name := capability.Method(i).Name
		acrossScopes := strings.HasSuffix(name, "AcrossScopes")
		ctx := scoped
		if acrossScopes {
			ctx = context.Background()
		}
		spy := New()
		mv := reflect.ValueOf(spy).MethodByName(name)
		args := make([]reflect.Value, mv.Type().NumIn())
		for j := range args {
			if pt := mv.Type().In(j); pt == ctxType {
				args[j] = reflect.ValueOf(ctx)
			} else {
				args[j] = reflect.Zero(pt)
			}
		}
		func() {
			defer func() {
				if recovered := recover(); recovered != nil {
					t.Errorf("Spy.%s fell through to the embedded nil Store: %v", name, recovered)
				}
			}()
			mv.Call(args)
		}()
		calls := spy.Calls()
		if len(calls) != 1 || calls[0].Method != name {
			t.Errorf("Spy.%s recorded %+v, want exactly one %s call", name, calls, name)
			continue
		}
		if !acrossScopes && calls[0].Scope.IsZero() {
			t.Errorf("Spy.%s recorded no scope", name)
		}
	}
}
