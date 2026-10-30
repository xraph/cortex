package postgres

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/xraph/grove"
	"github.com/xraph/grove/driver"
	"github.com/xraph/grove/drivers/pgdriver"
	"github.com/xraph/grove/drivers/pgdriver/pgmigrate"
	"github.com/xraph/grove/migrate"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
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

// TestMigrateConcurrentReplicasWithLegacyRows seeds rows written before
// v1.8.0 (no scope, conversations with no session), then starts eight
// replicas at once with one shared Rescoper. The rescope and the session
// backfill run after grove has released key 1, so only the cortex lock keeps
// replicas from running them side by side. Every replica must succeed, the
// Rescoper must be asked about each legacy identifier exactly once, every
// row must end up scoped, and each agent must get exactly one default
// session holding all of its conversation rows.
func TestMigrateConcurrentReplicasWithLegacyRows(t *testing.T) {
	ctx, dsn := startMigratePostgres(t)

	seed := New(openMigrateDB(ctx, t, dsn))
	if err := seed.Migrate(ctx); err != nil {
		t.Fatalf("schema migrate: %v", err)
	}

	const (
		apps           = 4
		agentsPerApp   = 2
		messagesPerRow = 3
	)
	agentIDs := make([]string, 0, apps*agentsPerApp)
	for a := range apps {
		app := fmt.Sprintf("app-%d", a)
		for n := range agentsPerApp {
			agentID := id.NewAgentID().String()
			agentIDs = append(agentIDs, agentID)
			if _, err := seed.pgdb.Exec(ctx, `INSERT INTO cortex_agents
    (id, name, app_id, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon, enabled)
    VALUES ($1, $2, $3, '', '', '', '{}', '', true)`, agentID, fmt.Sprintf("agent-%d", n), app); err != nil {
				t.Fatalf("insert legacy agent: %v", err)
			}
			for m := range messagesPerRow {
				if _, err := seed.pgdb.Exec(ctx, `INSERT INTO cortex_memories
    (agent_id, tenant_id, session_id, kind, content, metadata, scope_l0, scope_l1, scope_l2, scope_extra, scope_canon)
    VALUES ($1, $2, '', 'conversation', $3, '{}', '', '', '', '{}', '')`,
					agentID, app, fmt.Sprintf(`{"role":"user","content":"message %d"}`, m)); err != nil {
					t.Fatalf("insert legacy conversation row: %v", err)
				}
			}
		}
	}

	rescoper := &countingRescoper{delay: 100 * time.Millisecond, calls: make(map[[2]string]int)}
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
			errs[i] = s.Migrate(ctx, cortex.WithRescoper(rescoper))
		})
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("replica %d migrate: %v", i, err)
		}
	}

	calls := rescoper.snapshot()
	// Agents carry app_id and conversation rows carry tenant_id, so each
	// app reaches the Rescoper under two identifiers.
	if want := 2 * apps; len(calls) != want {
		t.Errorf("Rescoper saw %d distinct identifiers, want %d: %v", len(calls), want, calls)
	}
	for key, n := range calls {
		if n != 1 {
			t.Errorf("Rescoper called %d times for app=%q tenant=%q, want once", n, key[0], key[1])
		}
	}

	conn := connectPG(ctx, t, dsn)
	for _, q := range []struct{ what, sql string }{
		{"unscoped agents", `SELECT count(*) FROM cortex_agents WHERE scope_canon = ''`},
		{"unscoped memories", `SELECT count(*) FROM cortex_memories WHERE scope_canon = ''`},
		{"conversation rows without a session", `SELECT count(*) FROM cortex_memories WHERE kind = 'conversation' AND session_id = ''`},
		{"agents with more than one default session", `SELECT count(*) FROM (SELECT agent_id FROM cortex_sessions
			WHERE is_default GROUP BY agent_id, scope_canon HAVING count(*) > 1) d`},
		{"conversation rows outside their agent's default session", `SELECT count(*) FROM cortex_memories m
			WHERE m.kind = 'conversation' AND NOT EXISTS (SELECT 1 FROM cortex_sessions s
			WHERE s.id = m.session_id AND s.is_default AND s.agent_id = m.agent_id AND s.scope_canon = m.scope_canon)`},
	} {
		var n int
		if err := conn.QueryRow(ctx, q.sql).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", q.what, err)
		}
		if n != 0 {
			t.Errorf("%d %s, want 0", n, q.what)
		}
	}

	var sessions int
	if err := conn.QueryRow(ctx, `SELECT count(*) FROM cortex_sessions WHERE is_default`).Scan(&sessions); err != nil {
		t.Fatalf("count default sessions: %v", err)
	}
	if sessions != len(agentIDs) {
		t.Errorf("%d default sessions, want one per agent (%d)", sessions, len(agentIDs))
	}
	if n := countAdvisoryLocks(ctx, t, conn); n != 0 {
		t.Errorf("%d advisory locks left after every replica returned, want 0", n)
	}
}

// countingRescoper maps a legacy app or tenant to a workspace and counts
// how often each (appID, tenantID) pair is asked for. Each call takes delay,
// the way a Rescoper that looks the mapping up somewhere would; that keeps
// the rescope running longer than grove's key-1 retry staggers replicas, so
// overlapping passes would show. It is shared by every replica in a test, so
// it is safe for concurrent use.
type countingRescoper struct {
	delay time.Duration
	mu    sync.Mutex
	calls map[[2]string]int
}

func (r *countingRescoper) Rescope(ctx context.Context, appID, tenantID string) (cortex.Scope, error) {
	r.mu.Lock()
	r.calls[[2]string{appID, tenantID}]++
	r.mu.Unlock()
	if err := sleepCtx(ctx, r.delay); err != nil {
		return cortex.Scope{}, err
	}
	workspace := appID
	if workspace == "" {
		workspace = tenantID
	}
	return cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: workspace}}}, nil
}

func (r *countingRescoper) snapshot() map[[2]string]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return maps.Clone(r.calls)
}

// TestMigrateFailsFastOnPoolOfOne gives Migrate a pool of one connection,
// which it cannot run on, and expects a prompt error that says why rather
// than a hang. A pool of two is enough.
func TestMigrateFailsFastOnPoolOfOne(t *testing.T) {
	ctx, dsn := startMigratePostgres(t)

	done := make(chan error, 1)
	small := New(openMigrateDB(ctx, t, dsn, driver.WithPoolSize(1)))
	go func() { done <- small.Migrate(context.Background()) }()

	select {
	case err := <-done:
		if !errors.Is(err, errMigratePoolTooSmall) {
			t.Fatalf("Migrate on a pool of one = %v, want errMigratePoolTooSmall", err)
		}
	case <-time.After(migrateConnProbeTimeout + 10*time.Second):
		t.Fatal("Migrate on a pool of one did not return")
	}

	if err := New(openMigrateDB(ctx, t, dsn, driver.WithPoolSize(2))).Migrate(ctx); err != nil {
		t.Fatalf("Migrate on a pool of two: %v", err)
	}
}

// TestWithMigrateLockReleasesOnErrorAndPanic checks that the cortex lock is
// given back when the work under it fails and when it panics, so another
// session can take it straight away.
func TestWithMigrateLockReleasesOnErrorAndPanic(t *testing.T) {
	ctx, dsn := startMigratePostgres(t)
	s := New(openMigrateDB(ctx, t, dsn))
	observer := connectPG(ctx, t, dsn)

	assertReleased := func(t *testing.T) {
		t.Helper()
		if n := countAdvisoryLocks(ctx, t, observer); n != 0 {
			t.Fatalf("%d advisory locks left, want 0", n)
		}
		var acquired bool
		if err := observer.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", migrateLockKey).Scan(&acquired); err != nil {
			t.Fatalf("try cortex lock: %v", err)
		}
		if !acquired {
			t.Fatal("cortex lock still held by the Store's session")
		}
		if _, err := observer.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrateLockKey); err != nil {
			t.Fatalf("release cortex lock: %v", err)
		}
	}

	t.Run("error", func(t *testing.T) {
		boom := errors.New("boom")
		err := s.withMigrateLock(ctx, func(context.Context) error { return boom })
		if !errors.Is(err, boom) {
			t.Fatalf("withMigrateLock = %v, want the work's error", err)
		}
		assertReleased(t)
	})

	t.Run("panic", func(t *testing.T) {
		recovered := func() (r any) {
			defer func() { r = recover() }()
			_ = s.withMigrateLock(ctx, func(context.Context) error { panic("boom") })
			return nil
		}()
		if recovered != "boom" {
			t.Fatalf("recovered %v, want the work's panic to reach the caller", recovered)
		}
		assertReleased(t)
	})
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
func openMigrateDB(ctx context.Context, t *testing.T, dsn string, opts ...driver.Option) *grove.DB {
	t.Helper()
	drv := pgdriver.New()
	if err := drv.Open(ctx, dsn, opts...); err != nil {
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
