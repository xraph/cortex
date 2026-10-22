package postgres

import (
	"context"
	"errors"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xraph/grove/drivers/pgdriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/internal/state"
	"github.com/xraph/cortex/optimization/store/storetest"
)

func TestLeaseTakeoverAcrossProcesses(t *testing.T) {
	storetest.LeaseTakeoverAcrossProcesses(t, testDatabase, func(t *testing.T, dsn string, now func() time.Time) store.Store {
		t.Helper()
		s := New(openDatabase(t, dsn))
		if err := state.WithClock(now)(s.Store); err != nil {
			t.Fatal(err)
		}
		if err := s.Migrate(context.Background()); err != nil {
			t.Fatal(err)
		}
		return s
	})
}

func TestSameInstanceIndependentScopes(t *testing.T) {
	s := New(openDatabase(t, testDatabase(t)))
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	storetest.IndependentScopes(t, s)
}

func TestConcurrentMigrations(t *testing.T) {
	dsn := testDatabase(t)
	const workers = 8
	stores := make([]*Store, workers)
	for i := range stores {
		stores[i] = New(openDatabase(t, dsn))
	}
	start := make(chan struct{})
	results := make(chan error, workers)
	for _, s := range stores {
		go func() { <-start; results <- s.Migrate(context.Background()) }()
	}
	close(start)
	for range stores {
		if err := <-results; err != nil {
			t.Errorf("concurrent migration: %v", err)
		}
	}
	if err := stores[0].Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestIndependentClaimsIgnoreHostIsolationDefault(t *testing.T) {
	dsn, err := url.Parse(testDatabase(t))
	if err != nil {
		t.Fatal(err)
	}
	query := dsn.Query()
	query.Set("default_transaction_isolation", "repeatable read")
	dsn.RawQuery = query.Encode()
	firstDB := openDatabase(t, dsn.String())
	first, second := New(firstDB), New(openDatabase(t, dsn.String()))
	ctx := context.Background()
	if err = first.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	var hostIsolation string
	if err = pgdriver.Unwrap(firstDB).QueryRow(ctx, `SHOW default_transaction_isolation`).Scan(&hostIsolation); err != nil || hostIsolation != "repeatable read" {
		t.Fatalf("host default: %q, %v", hostIsolation, err)
	}
	for _, statement := range []string{
		`CREATE FUNCTION require_read_committed() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		 IF current_setting('transaction_isolation') <> 'read committed' THEN RAISE EXCEPTION 'wrong transaction isolation'; END IF;
		 RETURN NEW; END $$`,
		`CREATE TRIGGER require_read_committed BEFORE INSERT OR UPDATE ON cortex_optimization_records FOR EACH ROW EXECUTE FUNCTION require_read_committed()`,
	} {
		if _, err = pgdriver.Unwrap(firstDB).Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	bundle := storetest.Fixture(t, "postgres-claims")
	ctx = cortex.WithScope(ctx, bundle.Experiment.Spec.Scope)
	if err = first.Transact(ctx, func(tx store.Tx) error {
		if createErr := tx.CreateExperiment(bundle); createErr != nil {
			return createErr
		}
		return tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester})
	}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var calls atomic.Int32
	for i, s := range []*Store{first, second} {
		go func() {
			<-start
			results <- s.Transact(ctx, func(tx store.Tx) error {
				calls.Add(1)
				_, claimErr := tx.ClaimWork(optimization.ClaimRequest{ExperimentID: bundle.Experiment.ID, Owner: []string{"first", "second"}[i], Duration: 30 * time.Second})
				return claimErr
			})
		}()
	}
	close(start)
	winners, conflicts := 0, 0
	for range 2 {
		claimErr := <-results
		switch {
		case claimErr == nil:
			winners++
		case errors.Is(claimErr, optimization.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected claim error: %v", claimErr)
		}
	}
	if winners != 1 || conflicts != 1 || calls.Load() != 2 {
		t.Fatalf("claims: winners %d conflicts %d callbacks %d", winners, conflicts, calls.Load())
	}
	for _, s := range []*Store{first, second} {
		got, readErr := s.GetExperiment(ctx, bundle.Experiment.ID)
		if readErr != nil || got.Lease.Generation != 1 {
			t.Fatalf("stored lease: %+v, %v", got.Lease, readErr)
		}
	}
}

func TestScopeFenceAndCancelledWaiter(t *testing.T) {
	dsn := testDatabase(t)
	first, other, waiting := New(openDatabase(t, dsn)), New(openDatabase(t, dsn)), New(openDatabase(t, dsn))
	if err := first.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	a, b := storetest.Fixture(t, "scope-lock-a"), storetest.Fixture(t, "scope-lock-b")
	deadline, cancelFirst := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancelFirst()
	ctxA := cortex.WithScope(deadline, a.Experiment.Spec.Scope)
	ctxB := cortex.WithScope(context.Background(), b.Experiment.Spec.Scope)
	entered, release := make(chan struct{}), make(chan struct{})
	finished := make(chan error, 1)
	go func() {
		finished <- first.Transact(ctxA, func(tx store.Tx) error { close(entered); <-release; return tx.CreateExperiment(a) })
	}()
	select {
	case <-entered:
	case err := <-finished:
		t.Fatalf("transaction failed before callback: %v", err)
	}
	defer func() {
		close(release)
		if err := <-finished; err != nil {
			t.Error(err)
		}
	}()
	otherScope, cancelOther := context.WithTimeout(ctxB, 5*time.Second)
	defer cancelOther()
	if err := other.Transact(otherScope, func(tx store.Tx) error { return tx.CreateExperiment(b) }); err != nil {
		t.Fatalf("unrelated scope blocked: %v", err)
	}
	cancelled, cancel := context.WithTimeout(ctxA, 200*time.Millisecond)
	defer cancel()
	called := false
	err := waiting.Transact(cancelled, func(store.Tx) error { called = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("cancelled waiter: %v, callback %v", err, called)
	}
}

func TestDatabaseWriteFailureRollsBackAllRows(t *testing.T) {
	db := openDatabase(t, testDatabase(t))
	s := New(db)
	ctx := context.Background()
	if err := s.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE FUNCTION fail_second_optimizer_write() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN
		 IF (SELECT count(*) FROM cortex_optimization_records) = 1 THEN RAISE EXCEPTION 'injected second write failure'; END IF;
		 RETURN NEW; END $$`,
		`CREATE TRIGGER fail_second_optimizer_write BEFORE INSERT ON cortex_optimization_records FOR EACH ROW EXECUTE FUNCTION fail_second_optimizer_write()`,
	} {
		if _, err := pgdriver.Unwrap(db).Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	bundle := storetest.Fixture(t, "database-failure")
	ctx = cortex.WithScope(ctx, bundle.Experiment.Spec.Scope)
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err == nil {
		t.Fatal("database failure ignored")
	}
	var records, scopes int
	if err := pgdriver.Unwrap(db).QueryRow(ctx, `SELECT (SELECT count(*) FROM cortex_optimization_records), (SELECT count(*) FROM cortex_optimization_scopes)`).Scan(&records, &scopes); err != nil || records != 0 || scopes != 0 {
		t.Fatalf("partial commit: records %d scopes %d, %v", records, scopes, err)
	}
	if _, err := pgdriver.Unwrap(db).Exec(ctx, `DROP TRIGGER fail_second_optimizer_write ON cortex_optimization_records`); err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
		t.Fatal(err)
	}
	if _, err := pgdriver.Unwrap(db).Exec(ctx, `UPDATE cortex_optimization_schema SET version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); !errors.Is(err, optimization.ErrUnavailable) {
		t.Fatalf("future schema accepted: %v", err)
	}
}
