package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/sqlitedriver"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/optimization"
	"github.com/xraph/cortex/optimization/store"
	"github.com/xraph/cortex/optimization/store/storetest"
)

type processClaim struct {
	Database     string
	Result       string
	Scope        cortex.Scope
	ExperimentID id.ID
	Now          time.Time
}

func TestLeaseTakeoverAcrossProcesses(t *testing.T) {
	// A fresh test process has no access to the parent's Go locks or maps.
	if fixturePath := os.Getenv("CORTEX_OPTIMIZER_SQLITE_CLAIM"); fixturePath != "" {
		encoded, err := os.ReadFile(fixturePath)
		if err != nil {
			t.Fatal(err)
		}
		var request processClaim
		if err = json.Unmarshal(encoded, &request); err != nil {
			t.Fatal(err)
		}
		s := openTestStore(t, request.Database, func() time.Time { return request.Now })
		ctx := cortex.WithScope(context.Background(), request.Scope)
		var lease optimization.WorkLease
		if err = s.Transact(ctx, func(tx store.Tx) error {
			var claimErr error
			lease, claimErr = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: request.ExperimentID, Owner: "child", Duration: 30 * time.Second})
			return claimErr
		}); err != nil {
			t.Fatal(err)
		}
		encoded, err = json.Marshal(lease)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(request.Result, encoded, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	dir := t.TempDir()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, "optimizer.db")
	s := openTestStore(t, path, func() time.Time { return now })
	bundle := storetest.Fixture(t, "process")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	var old optimization.WorkLease
	if err := s.Transact(ctx, func(tx store.Tx) error {
		if err := tx.CreateExperiment(bundle); err != nil {
			return err
		}
		if err := tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester}); err != nil {
			return err
		}
		var err error
		old, err = tx.ClaimWork(optimization.ClaimRequest{ExperimentID: bundle.Experiment.ID, Owner: "parent", Duration: 30 * time.Second})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request := processClaim{Database: path, Result: filepath.Join(dir, "lease.json"), Scope: bundle.Experiment.Spec.Scope, ExperimentID: bundle.Experiment.ID, Now: now.Add(31 * time.Second)}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	fixturePath := filepath.Join(dir, "claim.json")
	if err = os.WriteFile(fixturePath, encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	deadline, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(deadline, executable, "-test.run=^TestLeaseTakeoverAcrossProcesses$")
	command.Env = append(os.Environ(), "CORTEX_OPTIMIZER_SQLITE_CLAIM="+fixturePath)
	if output, commandErr := command.CombinedOutput(); commandErr != nil {
		t.Fatalf("child: %v\n%s", commandErr, output)
	}
	encoded, err = os.ReadFile(request.Result)
	if err != nil {
		t.Fatal(err)
	}
	var replacement optimization.WorkLease
	if err = json.Unmarshal(encoded, &replacement); err != nil {
		t.Fatal(err)
	}
	if replacement.Generation != old.Generation+1 || replacement.Owner != "child" {
		t.Fatalf("invalid takeover: %+v", replacement)
	}
	if err = s.Transact(ctx, func(tx store.Tx) error { return tx.RenewLease(old) }); !errors.Is(err, optimization.ErrConflict) {
		t.Fatalf("old process renewed: %v", err)
	}
	got, err := s.GetExperiment(ctx, bundle.Experiment.ID)
	if err != nil || got.Lease != replacement {
		t.Fatalf("old connection has stale state: %+v, %v", got.Lease, err)
	}
}

func TestDatabaseFailureRollsBackRecordsAndAudit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "optimizer.db")
	s := openTestStore(t, path, time.Now)
	bundle := storetest.Fixture(t, "database-rollback")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	// Fail at the storage boundary, after the domain callback has succeeded.
	b := openRawTestDB(t, path)
	if _, err := b.Exec(ctx, `CREATE TRIGGER fail_optimizer_audit BEFORE INSERT ON cortex_optimization_records
	 WHEN (SELECT count(*) FROM cortex_optimization_records) = 1 BEGIN SELECT RAISE(ABORT, 'injected second write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err == nil {
		t.Fatal("injected write failure was ignored")
	}
	for _, kind := range []string{"experiments", "versions", "events"} {
		var count int
		if err := b.QueryRow(ctx, `SELECT count(*) FROM cortex_optimization_records WHERE kind = ?`, kind).Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial %s commit: %d, %v", kind, count, err)
		}
	}
	if _, err := b.Exec(ctx, `DROP TRIGGER fail_optimizer_audit`); err != nil {
		t.Fatal(err)
	}
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
		t.Fatal(err)
	}
}

func TestFutureSchemaAndCorruptRowsRefuse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "optimizer.db")
	s := openTestStore(t, path, time.Now)
	b := openRawTestDB(t, path)
	bundle := storetest.Fixture(t, "corrupt")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Exec(ctx, `UPDATE cortex_optimization_records SET body = '{}' WHERE kind = 'experiments'`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetExperiment(ctx, bundle.Experiment.ID); !errors.Is(err, optimization.ErrUnavailable) {
		t.Fatalf("corrupt read: %v", err)
	}
	called := false
	if err := s.Transact(ctx, func(store.Tx) error { called = true; return nil }); !errors.Is(err, optimization.ErrUnavailable) || called {
		t.Fatalf("corrupt transaction admitted: %v, called %v", err, called)
	}
	if _, err := b.Exec(ctx, `UPDATE cortex_optimization_schema SET version = 2`); err != nil {
		t.Fatal(err)
	}
	if err := s.Migrate(ctx); !errors.Is(err, optimization.ErrUnavailable) {
		t.Fatalf("future schema accepted: %v", err)
	}
}

func openRawTestDB(t *testing.T, path string) *sqlitedriver.SqliteDB {
	t.Helper()
	driver := sqlitedriver.New()
	if err := driver.Open(context.Background(), path+"?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"); err != nil {
		t.Fatal(err)
	}
	db, err := grove.Open(driver)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	return sqlitedriver.Unwrap(db)
}

func TestIndependentConnectionsClaimExactlyOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "optimizer.db")
	stores := []*Store{openTestStore(t, path, time.Now), openTestStore(t, path, time.Now)}
	bundle := storetest.Fixture(t, "claim-race")
	deadline, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctx := cortex.WithScope(deadline, bundle.Experiment.Spec.Scope)
	if err := stores[0].Transact(ctx, func(tx store.Tx) error {
		if err := tx.CreateExperiment(bundle); err != nil {
			return err
		}
		return tx.CommitTransition(optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester})
	}); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, len(stores))
	for i, s := range stores {
		go func() {
			<-start
			results <- s.Transact(ctx, func(tx store.Tx) error {
				_, err := tx.ClaimWork(optimization.ClaimRequest{ExperimentID: bundle.Experiment.ID, Owner: []string{"first", "second"}[i], Duration: 30 * time.Second})
				return err
			})
		}()
	}
	close(start)
	winners, conflicts := 0, 0
	for range stores {
		err := <-results
		switch {
		case err == nil:
			winners++
		case errors.Is(err, optimization.ErrConflict):
			conflicts++
		default:
			t.Fatalf("unexpected claim result: %v", err)
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("winners %d, conflicts %d", winners, conflicts)
	}
	for _, s := range stores {
		got, err := s.GetExperiment(ctx, bundle.Experiment.ID)
		if err != nil || got.Lease.Generation != 1 {
			t.Fatalf("lease generation: %+v, %v", got.Lease, err)
		}
	}
}

func TestIdempotentTransactionDoesNotRewriteHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "optimizer.db")
	s := openTestStore(t, path, time.Now)
	bundle := storetest.Fixture(t, "unchanged-records")
	ctx := cortex.WithScope(context.Background(), bundle.Experiment.Spec.Scope)
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CreateExperiment(bundle) }); err != nil {
		t.Fatal(err)
	}
	start := optimization.Transition{ExperimentID: bundle.Experiment.ID, ExpectedRevision: 1, NextState: optimization.StateQueued, IdempotencyKey: "start", Actor: bundle.Experiment.Spec.Requester}
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(start) }); err != nil {
		t.Fatal(err)
	}
	database := openRawTestDB(t, path)
	for _, statement := range []string{
		`CREATE TRIGGER forbid_optimizer_insert BEFORE INSERT ON cortex_optimization_records BEGIN SELECT RAISE(ABORT, 'unexpected insert'); END`,
		`CREATE TRIGGER forbid_optimizer_update BEFORE UPDATE ON cortex_optimization_records BEGIN SELECT RAISE(ABORT, 'unexpected update'); END`,
		`CREATE TRIGGER forbid_optimizer_delete BEFORE DELETE ON cortex_optimization_records BEGIN SELECT RAISE(ABORT, 'unexpected delete'); END`,
	} {
		if _, err := database.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Transact(ctx, func(tx store.Tx) error { return tx.CommitTransition(start) }); err != nil {
		t.Fatalf("retry rewrote records: %v", err)
	}
	if err := s.Transact(ctx, func(store.Tx) error { return nil }); err != nil {
		t.Fatalf("no-op rewrote records: %v", err)
	}
}
