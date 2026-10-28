package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/xraph/grove"
	"github.com/xraph/grove/drivers/pgdriver"
	"github.com/xraph/grove/drivers/pgdriver/pgmigrate"
	"github.com/xraph/grove/migrate"
)

// TestMigrateLockKeyDerivation pins migrateLockKey to its documented
// derivation and keeps it off grove's own advisory key, 1.
func TestMigrateLockKeyDerivation(t *testing.T) {
	h := fnv.New64a()
	_, _ = h.Write([]byte("github.com/xraph/cortex/store/postgres.Migrate"))
	want := int64(h.Sum64())

	if migrateLockKey != want {
		t.Fatalf("migrateLockKey = %d, want %d (FNV-1a 64 of the Migrate symbol)", migrateLockKey, want)
	}
	if migrateLockKey == 1 {
		t.Fatal("migrateLockKey collides with grove's advisory lock key 1")
	}
}

// TestMigrateGroupRetriesConcurrentCreate drives migrateGroup with an
// executor whose EnsureMigrationTable fails a set number of times, the way
// grove does when another grove user creates grove_migrations at the same
// moment. Only duplicate_table and duplicate_object are retried, at most
// migrateAttempts times.
func TestMigrateGroupRetriesConcurrentCreate(t *testing.T) {
	dupTable := fmt.Errorf("pgdriver: exec: %w", &pgconn.PgError{Code: "42P07", Message: `relation "grove_migrations" already exists`})
	dupObject := fmt.Errorf("pgdriver: exec: %w", &pgconn.PgError{Code: "42710", Message: `type "grove_migration_locks" already exists`})
	denied := fmt.Errorf("pgdriver: exec: %w", &pgconn.PgError{Code: "42501", Message: "permission denied"})

	tests := []struct {
		name      string
		failWith  error
		failures  int
		wantCalls int
		wantErr   error
	}{
		{"duplicate table twice then success", dupTable, 2, 3, nil},
		{"duplicate object once then success", dupObject, 1, 2, nil},
		{"other error is not retried", denied, 1, 1, denied},
		{"gives up after migrateAttempts", dupTable, migrateAttempts + 1, migrateAttempts, dupTable},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			exec := &racingExecutor{failWith: tt.failWith, failures: tt.failures}
			err := migrateGroup(context.Background(), exec)
			if tt.wantErr == nil && err != nil {
				t.Fatalf("migrateGroup: %v", err)
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Fatalf("migrateGroup = %v, want %v", err, tt.wantErr)
			}
			if exec.calls != tt.wantCalls {
				t.Fatalf("EnsureMigrationTable called %d times, want %d", exec.calls, tt.wantCalls)
			}
		})
	}
}

// racingExecutor fails EnsureMigrationTable failures times with failWith and
// then reports every cortex migration as applied, so the orchestrator has
// nothing to run. Methods the orchestrator does not reach in that case come
// from the nil embedded Executor and would panic if called.
type racingExecutor struct {
	migrate.Executor
	failWith error
	failures int
	calls    int
}

func (e *racingExecutor) EnsureMigrationTable(context.Context) error {
	e.calls++
	if e.calls <= e.failures {
		return e.failWith
	}
	return nil
}

func (e *racingExecutor) EnsureLockTable(context.Context) error     { return nil }
func (e *racingExecutor) AcquireLock(context.Context, string) error { return nil }
func (e *racingExecutor) ReleaseLock(context.Context) error         { return nil }
func (e *racingExecutor) ListApplied(context.Context) ([]*migrate.AppliedMigration, error) {
	migrations := Migrations.Migrations()
	applied := make([]*migrate.AppliedMigration, 0, len(migrations))
	for _, m := range migrations {
		applied = append(applied, &migrate.AppliedMigration{Version: m.Version, Name: m.Name, Group: m.Group})
	}
	return applied, nil
}

// TestMigrateConcurrentReplicas starts eight replicas, each with its own
// connection pool, on one fresh database and releases them at once. Every
// Migrate must succeed, grove_migrations must hold exactly one row per
// cortex version, and no advisory lock may outlive the calls.
func TestMigrateConcurrentReplicas(t *testing.T) {
	ctx, dsn := startMigratePostgres(t)

	const replicas = 8
	stores := make([]*Store, replicas)
	for i := range stores {
		stores[i] = New(openMigrateDB(ctx, t, dsn))
	}

	start := make(chan struct{})
	errs := make([]error, replicas)
	var wg sync.WaitGroup
	for i, s := range stores {
		wg.Go(func() {
			<-start
			errs[i] = s.Migrate(ctx)
		})
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d migrate: %v", i, err)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	conn := connectPG(ctx, t, dsn)

	rows, err := conn.Query(ctx, `SELECT version, count(*) FROM grove_migrations
		WHERE "group" = 'cortex' GROUP BY version HAVING count(*) > 1`)
	if err != nil {
		t.Fatalf("query duplicate versions: %v", err)
	}
	var dups []string
	for rows.Next() {
		var version string
		var n int
		if err := rows.Scan(&version, &n); err != nil {
			t.Fatalf("scan duplicate version: %v", err)
		}
		dups = append(dups, version)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("read duplicate versions: %v", err)
	}
	if len(dups) > 0 {
		t.Fatalf("versions recorded more than once: %v", dups)
	}

	var recorded int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM grove_migrations WHERE "group" = 'cortex'`).Scan(&recorded); err != nil {
		t.Fatalf("count recorded versions: %v", err)
	}
	if want := len(Migrations.Migrations()); recorded != want {
		t.Fatalf("grove_migrations holds %d cortex rows, want %d", recorded, want)
	}

	if n := countAdvisoryLocks(ctx, t, conn); n != 0 {
		t.Fatalf("%d advisory locks left after every replica returned, want 0", n)
	}
}

// TestMigrateAlongsideOtherGroveGroups races four cortex replicas against
// four plain grove migrators of an unrelated group on a fresh database, ten
// times. The others never take the cortex lock, only grove's key 1. Every
// cortex Migrate must succeed with one row per version and no deadlock; the
// other migrators may lose grove's own table race, which is theirs to retry.
func TestMigrateAlongsideOtherGroveGroups(t *testing.T) {
	ctx, dsn := startMigratePostgres(t)
	conn := connectPG(ctx, t, dsn)

	other := migrate.NewGroup("cortex_test_other")
	other.MustRegister(&migrate.Migration{
		Name:    "create_other",
		Version: "20260101000000",
		Up: func(ctx context.Context, exec migrate.Executor) error {
			_, err := exec.Exec(ctx, "CREATE TABLE IF NOT EXISTS cortex_test_other (id INT)")
			return err
		},
	})

	for round := range 10 {
		if _, err := conn.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public"); err != nil {
			t.Fatalf("round %d: reset schema: %v", round, err)
		}

		const pairs = 4
		dbs := make([]*grove.DB, 2*pairs)
		for i := range dbs {
			dbs[i] = openMigrateDB(ctx, t, dsn)
		}

		start := make(chan struct{})
		cortexErrs := make([]error, pairs)
		otherErrs := make([]error, pairs)
		var wg sync.WaitGroup
		for i := range pairs {
			wg.Go(func() {
				<-start
				cortexErrs[i] = New(dbs[i]).Migrate(ctx)
			})
			wg.Go(func() {
				<-start
				exec := pgmigrate.New(pgdriver.Unwrap(dbs[pairs+i]))
				_, otherErrs[i] = migrate.NewOrchestrator(exec, other).Migrate(ctx)
			})
		}
		close(start)
		wg.Wait()
		for _, db := range dbs {
			_ = db.Close()
		}

		for i, err := range cortexErrs {
			if err != nil {
				t.Errorf("round %d: cortex replica %d: %v", round, i, err)
			}
		}
		for i, err := range otherErrs {
			if err != nil && !isConcurrentCreate(err) {
				t.Errorf("round %d: other migrator %d: %v", round, i, err)
			}
		}
		if t.Failed() {
			t.FailNow()
		}

		var recorded int
		if err := conn.QueryRow(ctx, `SELECT count(*) FROM grove_migrations WHERE "group" = 'cortex'`).Scan(&recorded); err != nil {
			t.Fatalf("round %d: count recorded versions: %v", round, err)
		}
		if want := len(Migrations.Migrations()); recorded != want {
			t.Fatalf("round %d: grove_migrations holds %d cortex rows, want %d", round, recorded, want)
		}
		if n := countAdvisoryLocks(ctx, t, conn); n != 0 {
			t.Fatalf("round %d: %d advisory locks left, want 0", round, n)
		}
	}
}

// TestMigrateWaitsForHeldCortexLock holds the cortex migration lock from
// another session and expects Migrate to wait for it, then finish once it
// is released.
func TestMigrateWaitsForHeldCortexLock(t *testing.T) {
	ctx, dsn := startMigratePostgres(t)
	holder := connectPG(ctx, t, dsn)
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		t.Fatalf("hold cortex lock: %v", err)
	}

	s := New(openMigrateDB(ctx, t, dsn))
	done := make(chan error, 1)
	go func() { done <- s.Migrate(ctx) }()

	select {
	case err := <-done:
		t.Fatalf("Migrate returned while the cortex lock was held elsewhere: %v", err)
	case <-time.After(300 * time.Millisecond):
	}

	if _, err := holder.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrateLockKey); err != nil {
		t.Fatalf("release cortex lock: %v", err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Migrate after the lock was released: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Migrate did not return within 10s of the lock being released")
	}
}

// TestMigrateWaitsOutForeignAdvisoryLockOne holds advisory lock 1, the key
// every grove group shares, for a second from another session. Migrate has
// to wait for it rather than fail with grove's lock-held error.
func TestMigrateWaitsOutForeignAdvisoryLockOne(t *testing.T) {
	ctx, dsn := startMigratePostgres(t)
	holder := connectPG(ctx, t, dsn)
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock(1)"); err != nil {
		t.Fatalf("hold advisory lock 1: %v", err)
	}
	released := make(chan error, 1)
	go func() {
		time.Sleep(time.Second)
		_, err := holder.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock(1)")
		released <- err
	}()

	err := New(openMigrateDB(ctx, t, dsn)).Migrate(ctx)
	if err != nil {
		if strings.Contains(err.Error(), "lock is held by another process") {
			t.Fatalf("Migrate failed fast on a foreign lock 1: %v", err)
		}
		t.Fatalf("Migrate while lock 1 was held for a second: %v", err)
	}
	if err := <-released; err != nil {
		t.Fatalf("release advisory lock 1: %v", err)
	}
}

// TestMigrateCancelledWhileWaiting cancels a Migrate that is queued behind
// the cortex lock. It must return the context's error and leave no lock
// request of its own behind: only the holder's lock remains.
func TestMigrateCancelledWhileWaiting(t *testing.T) {
	ctx, dsn := startMigratePostgres(t)
	holder := connectPG(ctx, t, dsn)
	if _, err := holder.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLockKey); err != nil {
		t.Fatalf("hold cortex lock: %v", err)
	}
	holderPID := holder.PgConn().PID()

	s := New(openMigrateDB(ctx, t, dsn))
	waitCtx, cancel := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancel()

	err := s.Migrate(waitCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Migrate with an expired context = %v, want context.DeadlineExceeded", err)
	}

	observer := connectPG(ctx, t, dsn)
	deadline := time.Now().Add(2 * time.Second)
	for {
		pids := advisoryLockPIDs(ctx, t, observer)
		if len(pids) == 1 && pids[0] == holderPID {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("advisory locks held or awaited by pids %v, want only the holder %d", pids, holderPID)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// startMigratePostgres starts a throwaway PostgreSQL 17 container for one
// test and returns a context bounded for that test with the container's DSN.
func startMigratePostgres(t *testing.T) (context.Context, string) {
	t.Helper()
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)

	pgContainer, err := tcpostgres.Run(ctx, "postgres:17-alpine",
		tcpostgres.WithDatabase("cortex_migrate_lock"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start postgres container: %v", err)
	}
	t.Cleanup(func() {
		if cleanupErr := pgContainer.Terminate(context.Background()); cleanupErr != nil {
			t.Logf("terminate postgres container: %v", cleanupErr)
		}
	})

	dsn, err := pgContainer.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("postgres connection string: %v", err)
	}
	return ctx, dsn
}

// openMigrateDB opens a grove database with a connection pool of its own,
// the way a separate replica would.
func openMigrateDB(ctx context.Context, t *testing.T, dsn string) *grove.DB {
	t.Helper()
	drv := pgdriver.New()
	if err := drv.Open(ctx, dsn); err != nil {
		t.Fatalf("open pg driver: %v", err)
	}
	db, err := grove.Open(drv)
	if err != nil {
		t.Fatalf("grove open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// connectPG opens one plain session, outside any pool, for holding locks
// and inspecting pg_locks.
func connectPG(ctx context.Context, t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect postgres: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// countAdvisoryLocks counts advisory locks held or awaited in the current
// database.
func countAdvisoryLocks(ctx context.Context, t *testing.T, conn *pgx.Conn) int {
	t.Helper()
	return len(advisoryLockPIDs(ctx, t, conn))
}

// advisoryLockPIDs lists the backend of every advisory lock held or awaited
// in the current database, granted or not.
func advisoryLockPIDs(ctx context.Context, t *testing.T, conn *pgx.Conn) []uint32 {
	t.Helper()
	rows, err := conn.Query(ctx, `SELECT pid FROM pg_locks
		WHERE locktype = 'advisory'
		  AND database = (SELECT oid FROM pg_database WHERE datname = current_database())
		ORDER BY pid`)
	if err != nil {
		t.Fatalf("query pg_locks: %v", err)
	}
	defer rows.Close()
	var pids []uint32
	for rows.Next() {
		var pid int32
		if err := rows.Scan(&pid); err != nil {
			t.Fatalf("scan pg_locks: %v", err)
		}
		pids = append(pids, uint32(pid))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read pg_locks: %v", err)
	}
	return pids
}
