package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/run"
	"github.com/xraph/cortex/store/scopespy"
	"github.com/xraph/cortex/suspension"
)

// sweepStore is scopespy.Spy plus the two methods only the sweeper
// calls. Spy models the resume claim and nothing about expiry, so the
// pair is added here rather than there: adding them to Spy would put two
// overrides in the shared double that no scenario in its own
// completeness test reaches.
//
// The claim is modelled off the stored run state, the way every real
// backend does it, rather than off a bookkeeping flag. That is the
// property the sweeper depends on, so a double that faked it would make
// these tests prove nothing.
type sweepStore struct {
	*scopespy.Spy

	mu             sync.Mutex
	crossScopeCtxs []cortex.Scope
	claimScopes    []cortex.Scope

	// onList runs at the top of every cross-scope list, so a test can
	// hold a sweep open and observe what happens around it.
	onList func()

	// sweptWhileHeld is written by the sweeper goroutine and read by the
	// test only after Stop returns. It is a plain field on purpose: if
	// Stop joins, the join is the happens-before edge that makes the
	// read safe, and if it does not, the race detector says so.
	sweptWhileHeld bool
}

func newSweepStore() *sweepStore {
	return &sweepStore{Spy: scopespy.New()}
}

// ListExpiredAcrossScopes ignores whatever scope the context carries, as
// the real backends do, and records what it was given so a test can
// prove the sweeper is not quietly relying on one.
func (s *sweepStore) ListExpiredAcrossScopes(ctx context.Context, now time.Time, limit int) ([]*suspension.Suspension, error) {
	if s.onList != nil {
		s.onList()
	}
	s.mu.Lock()
	s.crossScopeCtxs = append(s.crossScopeCtxs, cortex.ScopeFromContext(ctx))
	s.mu.Unlock()

	var out []*suspension.Suspension
	for _, susp := range s.Suspensions() {
		if susp.ExpiresAt == nil || susp.ExpiresAt.After(now) {
			continue
		}
		out = append(out, susp)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// ClaimExpiredSuspension is the real contract in miniature: the run must
// still be paused and the deadline must have passed, or the claim is
// refused and nothing moves.
func (s *sweepStore) ClaimExpiredSuspension(ctx context.Context, runID id.AgentRunID, now time.Time) (*suspension.Suspension, error) {
	s.mu.Lock()
	s.claimScopes = append(s.claimScopes, cortex.ScopeFromContext(ctx))
	s.mu.Unlock()

	if cortex.ScopeFromContext(ctx).IsZero() {
		return nil, cortex.ErrNoScope
	}

	susp, err := s.GetSuspension(ctx, runID)
	if err != nil {
		return nil, err
	}
	if susp.ExpiresAt == nil || susp.ExpiresAt.After(now) {
		return nil, cortex.ErrNotSuspended
	}

	r, err := s.GetRun(ctx, runID)
	if err != nil {
		return nil, err
	}
	if r.State != run.StatePaused {
		return nil, cortex.ErrNotSuspended
	}
	r.State = run.StateRunning
	if err := s.UpdateRun(ctx, r); err != nil {
		return nil, err
	}
	return susp, nil
}

func (s *sweepStore) crossScopeCalls() []cortex.Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]cortex.Scope, len(s.crossScopeCtxs))
	copy(out, s.crossScopeCtxs)
	return out
}

func (s *sweepStore) claimCalls() []cortex.Scope {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]cortex.Scope, len(s.claimScopes))
	copy(out, s.claimScopes)
	return out
}

// sweepFixture writes one paused run and the suspension that belongs to
// it, with the given deadline and under the given scope. Everything
// except those two is identical from one fixture to the next, which is
// what lets a test attribute a difference in outcome to the deadline
// alone.
func sweepFixture(t *testing.T, s *sweepStore, scope cortex.Scope, expiresAt *time.Time) *run.Run {
	t.Helper()
	ctx := cortex.WithScope(context.Background(), scope)

	r := &run.Run{
		Entity:  cortex.NewEntity(),
		ID:      id.NewAgentRunID(),
		AgentID: id.NewAgentID(),
		Scope:   scope,
		State:   run.StatePaused,
		Input:   "waiting on the outside world",
	}
	if err := s.CreateRun(ctx, r); err != nil {
		t.Fatalf("fixture: create run: %v", err)
	}
	susp := &suspension.Suspension{
		Entity:    cortex.NewEntity(),
		ID:        id.NewSuspensionID(),
		RunID:     r.ID,
		Scope:     scope,
		Reason:    suspension.ReasonExternalTool,
		Pending:   []suspension.PendingCall{{ID: "call-1", Name: "ask_human"}},
		ExpiresAt: expiresAt,
	}
	if err := s.CreateSuspension(ctx, susp); err != nil {
		t.Fatalf("fixture: create suspension: %v", err)
	}
	return r
}

func past() *time.Time {
	t := time.Now().UTC().Add(-time.Hour)
	return &t
}

func future() *time.Time {
	t := time.Now().UTC().Add(time.Hour)
	return &t
}

func stateOf(t *testing.T, s *sweepStore, scope cortex.Scope, runID id.AgentRunID) run.State {
	t.Helper()
	r, err := s.GetRun(cortex.WithScope(context.Background(), scope), runID)
	if err != nil {
		t.Fatalf("reload run: %v", err)
	}
	return r.State
}

func hasSuspension(s *sweepStore, runID id.AgentRunID) bool {
	for _, susp := range s.Suspensions() {
		if susp.RunID == runID {
			return true
		}
	}
	return false
}

// TestSweepOnce_FailsThePastDeadlineAndLeavesTheFutureOneAlone is the
// sweeper's whole contract in one pass.
//
// The two fixtures differ in exactly one thing: the deadline. Same
// scope, same reason, same pending call, same paused run. If the swept
// one had also been the only external-tool suspension, or the only one
// in its scope, a sweeper keying off either of those would pass this
// test while failing the wrong runs in production.
func TestSweepOnce_FailsThePastDeadlineAndLeavesTheFutureOneAlone(t *testing.T) {
	s := newSweepStore()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}
	expired := sweepFixture(t, s, scope, past())
	waiting := sweepFixture(t, s, scope, future())

	e, err := New(WithStore(s))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// The sweeper's own context: no scope on it, because a sweeper never
	// has one.
	e.sweepOnce(context.Background())

	if got := stateOf(t, s, scope, expired.ID); got != run.StateFailed {
		t.Errorf("expired run state = %q, want %q; a run nobody resumed stays paused forever", got, run.StateFailed)
	}
	if hasSuspension(s, expired.ID) {
		t.Error("the expired suspension survived the sweep; the row is claimed by nothing and read by nobody")
	}

	if got := stateOf(t, s, scope, waiting.ID); got != run.StatePaused {
		t.Errorf("run whose deadline is still ahead = %q, want %q; the sweep failed a run somebody can still answer", got, run.StatePaused)
	}
	if !hasSuspension(s, waiting.ID) {
		t.Error("the sweep deleted a suspension whose deadline had not passed")
	}

	// The failure has to be matchable, not just present. A caller
	// distinguishing "the tool errored" from "nobody came back" reads
	// this field.
	r, err := s.GetRun(cortex.WithScope(context.Background(), scope), expired.ID)
	if err != nil {
		t.Fatalf("reload the expired run: %v", err)
	}
	if r.Error == "" {
		t.Fatal("the expired run carries no error; whoever finds it has nothing to go on")
	}
	if !strings.Contains(r.Error, cortex.ErrSuspensionExpired.Error()) {
		t.Errorf("expired run error = %q, want it to carry %q", r.Error, cortex.ErrSuspensionExpired)
	}
}

// TestSweepOnce_WorksUnderTheSuspensionsOwnScope pins the half of the
// design the cross-scope read does not cover. Finding work everywhere
// does not license doing it nowhere in particular: each run's writes
// have to land in the scope that run started under.
func TestSweepOnce_WorksUnderTheSuspensionsOwnScope(t *testing.T) {
	s := newSweepStore()
	scopeA := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_a"}}}
	scopeB := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_b"}}}
	runA := sweepFixture(t, s, scopeA, past())
	runB := sweepFixture(t, s, scopeB, past())

	e, err := New(WithStore(s))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.sweepOnce(context.Background())

	for _, c := range []struct {
		name  string
		scope cortex.Scope
		runID id.AgentRunID
	}{
		{name: "ws_a", scope: scopeA, runID: runA.ID},
		{name: "ws_b", scope: scopeB, runID: runB.ID},
	} {
		if got := stateOf(t, s, c.scope, c.runID); got != run.StateFailed {
			t.Errorf("%s: run state = %q, want %q; one sweep must reach every scope", c.name, got, run.StateFailed)
		}
	}

	// The list is asked without a scope; the claims are made with one.
	for i, sc := range s.crossScopeCalls() {
		if !sc.IsZero() {
			t.Errorf("cross-scope list %d carried scope %v; the sweeper has no request scope to pass", i, sc)
		}
	}
	claims := s.claimCalls()
	if len(claims) != 2 {
		t.Fatalf("%d claims, want 2 (one per expired suspension)", len(claims))
	}
	seen := map[string]bool{}
	for _, sc := range claims {
		if sc.IsZero() {
			t.Fatal("a claim was made with no scope; the per-run work must run under the suspension's own scope")
		}
		seen[sc.Canonical()] = true
	}
	if !seen[scopeA.Canonical()] || !seen[scopeB.Canonical()] {
		t.Errorf("claims were made under %v, want one under each of ws_a and ws_b", seen)
	}
}

// TestSweepOnce_SkipsARunAResumeAlreadyClaimed is the race with Resume,
// written as the interleaving that actually happens: a resume beats the
// deadline, claims the run, and is still executing when the deadline
// passes and the sweeper comes past. The suspension row is still there
// (a resume holds it until its own delete), so the sweep sees it and
// must refuse it on the run's state.
//
// Without the claim the sweep would fail a run that is mid-flight, and
// the run would carry on executing under a terminal state nothing put
// it in.
func TestSweepOnce_SkipsARunAResumeAlreadyClaimed(t *testing.T) {
	s := newSweepStore()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}
	r := sweepFixture(t, s, scope, past())

	// What a resume's ClaimSuspension leaves behind: the run running,
	// the suspension row still present.
	ctx := cortex.WithScope(context.Background(), scope)
	claimed, err := s.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatalf("load the run to claim it: %v", err)
	}
	claimed.State = run.StateRunning
	if upErr := s.UpdateRun(ctx, claimed); upErr != nil {
		t.Fatalf("claim the run: %v", upErr)
	}

	e, err := New(WithStore(s))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	e.sweepOnce(context.Background())

	if got := stateOf(t, s, scope, r.ID); got != run.StateRunning {
		t.Errorf("run state after the sweep = %q, want %q; the sweep failed a run a resume already owns", got, run.StateRunning)
	}
	if !hasSuspension(s, r.ID) {
		t.Error("the sweep deleted the suspension out from under a resume in flight; the resume's own delete will now fail its run")
	}
}

// TestWithSuspensionTTL_ZeroWritesNoDeadlineAndRunsNoSweeper covers the
// disable switch on both the write side and the lifecycle side, because
// either one alone leaves expiry half off: nil deadlines with a sweeper
// running is merely wasteful, but a sweeper running against deadlines
// somebody thought were switched off is a run failed by a feature its
// host opted out of.
func TestWithSuspensionTTL_ZeroWritesNoDeadlineAndRunsNoSweeper(t *testing.T) {
	t.Run("suspensions are written with no deadline", func(t *testing.T) {
		r, spy := suspendingRun(t, WithSuspensionTTL(0))
		if r.State != run.StatePaused {
			t.Fatalf("run state = %q, want %q", r.State, run.StatePaused)
		}
		susps := spy.Suspensions()
		if len(susps) != 1 {
			t.Fatalf("%d suspensions written, want 1", len(susps))
		}
		if susps[0].ExpiresAt != nil {
			t.Errorf("ExpiresAt = %v, want nil; expiry is off, so nothing should have a deadline", susps[0].ExpiresAt)
		}
	})

	t.Run("the default TTL does write one", func(t *testing.T) {
		// The mirror, so the assertion above is about the option rather
		// than about the field never being set at all.
		_, spy := suspendingRun(t)
		susps := spy.Suspensions()
		if len(susps) != 1 {
			t.Fatalf("%d suspensions written, want 1", len(susps))
		}
		if susps[0].ExpiresAt == nil {
			t.Fatal("ExpiresAt = nil under the default TTL; nothing would ever be swept")
		}
		if got := time.Until(*susps[0].ExpiresAt); got > defaultSuspensionTTL || got < defaultSuspensionTTL-time.Minute {
			t.Errorf("deadline is %s out, want about %s", got, defaultSuspensionTTL)
		}
	})

	t.Run("no sweeper goroutine starts", func(t *testing.T) {
		s := newSweepStore()
		scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}
		// A row that IS expired, so a sweeper that ran would visibly
		// take it. Asserting on an unexpired row would pass against a
		// sweeper that ran and found nothing.
		r := sweepFixture(t, s, scope, past())

		e, err := New(WithStore(s), WithSuspensionTTL(0), WithSuspensionSweepInterval(time.Millisecond))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		if err := e.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		// Long enough for a millisecond ticker to have fired many times
		// if one had been started at all.
		time.Sleep(50 * time.Millisecond)
		if err := e.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		if n := len(s.crossScopeCalls()); n != 0 {
			t.Errorf("the sweeper listed expired suspensions %d times with expiry switched off", n)
		}
		if got := stateOf(t, s, scope, r.ID); got != run.StatePaused {
			t.Errorf("run state = %q, want %q; expiry is off and this run was failed anyway", got, run.StatePaused)
		}
	})
}

// TestWithSuspensionTTL_RefusesANegativeDuration keeps a typo from
// switching the sweeper into a mode where every suspension is already
// expired the moment it is written.
func TestWithSuspensionTTL_RefusesANegativeDuration(t *testing.T) {
	if _, err := New(WithSuspensionTTL(-time.Hour)); err == nil {
		t.Fatal("New accepted a negative suspension TTL; every run would be swept on the next tick")
	}
}

// TestStartStop_TheSweeperRunsAndStopJoinsIt covers the lifecycle
// itself: Start's context is discarded, so the loop needs a handle of
// its own, and Stop has to WAIT for the goroutine rather than merely
// signal it.
//
// The join is asserted by reading, immediately after Stop returns, the
// state the sweeper writes. Under -race a Stop that only signalled would
// report the read against the sweeper's own write.
func TestStartStop_TheSweeperRunsAndStopJoinsIt(t *testing.T) {
	s := newSweepStore()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}
	r := sweepFixture(t, s, scope, past())

	e, err := New(WithStore(s), WithSuspensionSweepInterval(time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for stateOf(t, s, scope, r.ID) != run.StateFailed {
		if time.Now().After(deadline) {
			t.Fatal("the sweeper never failed an expired run; Start did not launch it")
		}
		time.Sleep(time.Millisecond)
	}

	if err := e.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// Nothing may move after Stop returns. A signalled-but-unjoined
	// goroutine gets one more tick in, which this catches by count.
	before := len(s.crossScopeCalls())
	time.Sleep(50 * time.Millisecond)
	if after := len(s.crossScopeCalls()); after != before {
		t.Errorf("the sweeper swept %d more times after Stop returned; Stop signalled without joining", after-before)
	}
}

// TestStop_WaitsForASweepAlreadyInFlight is the join itself, and it is
// the one property a Stop that merely signalled would still pass every
// other test in this file with: cancelling the context makes the loop
// exit almost immediately, so "nothing swept after Stop" is true either
// way once the sweeper is idle.
//
// So the sweeper is not idle. The store holds the sweep open inside its
// list call, the test releases it only after Stop has been entered, and
// the assertion is on work the sweeper does AFTER that release. A Stop
// that returned on the signal alone would read the flag before the
// sweeper ever set it, and under -race it would also be reading a field
// the sweeper goroutine is concurrently writing.
func TestStop_WaitsForASweepAlreadyInFlight(t *testing.T) {
	s := newSweepStore()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	s.onList = func() {
		once.Do(func() { close(entered) })
		<-release
		s.sweptWhileHeld = true
	}

	e, err := New(WithStore(s), WithSuspensionSweepInterval(time.Millisecond))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := e.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// The sweeper is now inside a sweep and cannot get out of it.
	<-entered
	go func() {
		time.Sleep(20 * time.Millisecond)
		close(release)
	}()

	if err := e.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !s.sweptWhileHeld {
		t.Fatal("Stop returned while a sweep was still running; it signalled the goroutine instead of joining it")
	}
}

// TestStartStop_AreIdempotent covers the two calls a host actually
// makes by accident: a second Start (which must not leave two loops
// failing the same runs) and a second Stop (which must not block on a
// channel nothing will close).
func TestStartStop_AreIdempotent(t *testing.T) {
	s := newSweepStore()
	e, err := New(WithStore(s), WithSuspensionSweepInterval(time.Hour))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for range 2 {
		if err := e.Start(context.Background()); err != nil {
			t.Fatalf("Start: %v", err)
		}
	}
	for range 2 {
		if err := e.Stop(context.Background()); err != nil {
			t.Fatalf("Stop: %v", err)
		}
	}
}

// TestExpireSuspension_ReportsAStoreFailureAndKeepsTheRow pins what
// happens when the terminal write does not land. The suspension is the
// only record of what the run was waiting on, so it survives for the
// next sweep to try again rather than being dropped on a run still
// marked running.
func TestExpireSuspension_ReportsAStoreFailureAndKeepsTheRow(t *testing.T) {
	s := newSweepStore()
	scope := cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "ws_x"}}}
	r := sweepFixture(t, s, scope, past())

	e, err := New(WithStore(&failingUpdateStore{sweepStore: s, err: errors.New("store is down")}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	susps := s.Suspensions()
	if len(susps) != 1 {
		t.Fatalf("%d suspensions, want 1", len(susps))
	}
	if err := e.expireSuspension(context.Background(), susps[0], time.Now().UTC()); err == nil {
		t.Fatal("expireSuspension reported success against a store that could not write the failure")
	}
	if !hasSuspension(s, r.ID) {
		t.Error("the suspension was deleted even though the run was never failed; nothing records what it was waiting on")
	}
}

// failingUpdateStore fails the run state write and nothing else, so a
// test can reach the one exit expireSuspension cannot recover from.
type failingUpdateStore struct {
	*sweepStore
	err error
}

func (s *failingUpdateStore) UpdateRun(ctx context.Context, r *run.Run) error {
	// The claim's own transition has to land, or the test never reaches
	// the write it is about.
	if r.State == run.StateRunning {
		return s.sweepStore.UpdateRun(ctx, r)
	}
	return s.err
}
