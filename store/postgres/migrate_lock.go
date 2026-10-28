package postgres

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/xraph/grove/driver"
	"github.com/xraph/grove/migrate"
)

// migrateLockKey is the session-level advisory lock Migrate holds while it
// runs. It is the FNV-1a 64-bit hash of
// "github.com/xraph/cortex/store/postgres.Migrate" (0xf340e9a7d5a2e83c)
// reinterpreted as an int64, so it never collides with key 1, which every
// grove migration group on a database shares.
const migrateLockKey int64 = -918477416930547652

// withMigrateLock runs fn while this process holds the cortex migration lock.
// The lock is taken and released on one dedicated connection, so it cannot
// leak to another pooled session. Taking it waits for any other replica
// inside Migrate; cancelling ctx abandons the wait.
//
// fn runs on other pooled connections (grove's executor takes one for its
// own lock), so Migrate needs at least two connections in the pool.
func (s *Store) withMigrateLock(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	conn, err := s.pgdb.AcquireConn(ctx)
	if err != nil {
		return fmt.Errorf("cortex: acquire migration lock: %w", err)
	}
	defer conn.Release()

	if lockErr := waitForMigrateLock(ctx, conn); lockErr != nil {
		return fmt.Errorf("cortex: acquire migration lock: %w", lockErr)
	}
	defer func() {
		if unlockErr := releaseMigrateLock(context.WithoutCancel(ctx), conn); unlockErr != nil {
			err = errors.Join(err, unlockErr)
		}
	}()

	return fn(ctx)
}

// Polling bounds for waitForMigrateLock: the first retry comes after
// migrateLockMinWait and the pause doubles up to migrateLockMaxWait.
const (
	migrateLockMinWait = 50 * time.Millisecond
	migrateLockMaxWait = time.Second
)

// waitForMigrateLock retries pg_try_advisory_lock on conn until it holds the
// cortex lock or ctx ends.
//
// It polls rather than blocking in pg_advisory_lock on purpose. A session
// blocked in pg_advisory_lock keeps its statement, and so its snapshot, open
// for the whole wait. CREATE INDEX CONCURRENTLY, which several cortex
// migrations run, waits for every older snapshot to go away, so the holder
// would wait on the replicas queued behind it and none of them would finish.
// Each try here is a short statement that holds nothing while it sleeps.
func waitForMigrateLock(ctx context.Context, conn driver.DedicatedConn) error {
	wait := migrateLockMinWait
	for {
		var acquired bool
		if err := conn.QueryRow(ctx, "SELECT pg_try_advisory_lock($1)", migrateLockKey).Scan(&acquired); err != nil {
			return withContextErr(ctx, err)
		}
		if acquired {
			return nil
		}
		if err := sleepCtx(ctx, jitter(wait)); err != nil {
			return err
		}
		wait = min(2*wait, migrateLockMaxWait)
	}
}

// jitter returns a duration between d/2 and d, so replicas waiting together
// spread out their retries.
func jitter(d time.Duration) time.Duration {
	half := d / 2
	return half + rand.N(half+1) //nolint:gosec // jitter does not need a cryptographic source
}

// sleepCtx waits for d or until ctx ends, whichever comes first.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// migrateAttempts bounds how many times migrateGroup runs grove's orchestrator
// when it loses the race on grove's tracking tables.
const migrateAttempts = 5

// migrateGroup applies the pending cortex migrations through grove's
// orchestrator on exec.
//
// Grove creates grove_migrations and grove_migration_locks before it takes
// its own lock. The cortex lock keeps cortex replicas apart, but another grove
// user on the same database (a second library, or a host's central pass)
// never takes it, so on a fresh database the two can still race on that
// CREATE and one fails with duplicate_table or duplicate_object. migrateGroup
// retries that failure a few times with a short jittered pause; by the next
// attempt the tables exist. Any other error is returned at once.
func migrateGroup(ctx context.Context, exec migrate.Executor) error {
	for attempt := 1; ; attempt++ {
		_, err := migrate.NewOrchestrator(exec, Migrations).Migrate(ctx)
		if err == nil || attempt == migrateAttempts || !isConcurrentCreate(err) {
			return err
		}
		if waitErr := sleepCtx(ctx, jitter(time.Duration(attempt)*100*time.Millisecond)); waitErr != nil {
			return errors.Join(err, waitErr)
		}
	}
}

// isConcurrentCreate reports duplicate_table (42P07) or duplicate_object
// (42710), what Postgres raises when two sessions run the same CREATE TABLE
// IF NOT EXISTS at once and one of them loses.
func isConcurrentCreate(err error) bool {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	return pgErr.Code == pgDuplicateTable || pgErr.Code == pgDuplicateObject
}

// SQLSTATE codes isConcurrentCreate matches.
const (
	pgDuplicateTable  = "42P07"
	pgDuplicateObject = "42710"
)

// releaseMigrateLock gives the cortex lock back on the connection that took
// it. If the unlock fails it falls back to pg_advisory_unlock_all, as grove's
// pgmigrate does, so the connection goes back to the pool holding nothing.
func releaseMigrateLock(ctx context.Context, conn driver.DedicatedConn) error {
	_, err := conn.Exec(ctx, "SELECT pg_advisory_unlock($1)", migrateLockKey)
	if err == nil {
		return nil
	}
	if _, allErr := conn.Exec(ctx, "SELECT pg_advisory_unlock_all()"); allErr != nil {
		return fmt.Errorf("cortex: release migration lock: %w", errors.Join(err, allErr))
	}
	return nil
}

// withContextErr makes sure an error caused by ctx ending matches ctx.Err()
// under errors.Is, whatever the driver wrapped it in.
func withContextErr(ctx context.Context, err error) error {
	ctxErr := ctx.Err()
	if ctxErr == nil || errors.Is(err, ctxErr) {
		return err
	}
	return fmt.Errorf("%w: %w", ctxErr, err)
}
