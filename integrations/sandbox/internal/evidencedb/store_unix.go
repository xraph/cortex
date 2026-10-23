//go:build linux || darwin

// Package evidencedb owns a private SQLite evidence store for the broker.
package evidencedb

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sync"

	"github.com/xraph/grove"
	"github.com/xraph/grove/driver"
	"github.com/xraph/grove/drivers/sqlitedriver"
	_ "github.com/xraph/grove/drivers/sqlitedriver/sqlitemigrate" // Cortex SQLite migrations.
	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
)

const applicationID uint32 = 0x43584556
const formatVersion uint32 = 1

var (
	_ evidence.Storage         = (*Store)(nil)
	_ evidence.TombstoneReader = (*Store)(nil)
)

// Store exposes only evidence persistence and audit. The service repository
// supplies authorization. Close waits for active operations before releasing
// ownership; this type must not be copied after use.
type Store struct {
	mu       sync.RWMutex
	data     *sqlitestore.Store
	file     *os.File
	lock     *os.File
	closeErr error
}

// Open opens a root-owned database in an existing private directory. It rejects
// aliases, unsafe sidecars, foreign application/format headers and another owner.
// A separate ownership file covers startup; SQLite also uses exclusive locking.
// New files receive the evidence identity before Grove enables WAL. Migrations
// reuse Cortex's SQLite store. The trusted owner must not replace files during
// use; local SQLite storage must support file locks and synchronous writes.
func Open(ctx context.Context, name string) (*Store, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("cortex: installed evidence storage requires root")
	}
	return open(ctx, name, 0)
}

func open(ctx context.Context, name string, owner uint32) (_ *Store, err error) {
	if operationErr := ctx.Err(); operationErr != nil {
		return nil, operationErr
	}
	lock, err := hostfs.OpenPrivateState(name+".lock", owner, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, lock.Close())
		}
	}()
	lockInfo, err := lock.Stat()
	if err != nil {
		return nil, err
	}
	if lockInfo.Size() != 0 {
		return nil, fmt.Errorf("cortex: evidence ownership file is not empty")
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return nil, fmt.Errorf("cortex: evidence database is already owned: %w", err)
	}
	file, err := hostfs.OpenPrivateState(name, owner, true)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, file.Close())
		}
	}()
	empty, err := checkFiles(name, file, owner)
	if err != nil {
		return nil, err
	}
	if empty {
		if initializeErr := initialize(ctx, name); initializeErr != nil {
			return nil, initializeErr
		}
	}
	data, err := connect(ctx, name)
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, data.Close())
		}
	}()
	if _, err = checkFiles(name, file, owner); err != nil {
		return nil, err
	}
	if _, err = checkIdentity(name+".lock", lock, owner); err != nil {
		return nil, err
	}
	directory, err := hostfs.OpenDirectory(filepath.Dir(name), owner)
	if err != nil {
		return nil, err
	}
	err = errors.Join(directory.Sync(), directory.Close(), ctx.Err())
	if err != nil {
		return nil, err
	}
	return &Store{data: data, file: file, lock: lock}, nil
}

func checkIdentity(name string, file *os.File, owner uint32) (unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return stat, err
	}
	actual, err := hostfs.StatPrivateState(name, owner)
	if err != nil {
		return stat, err
	}
	if stat.Dev != actual.Dev || stat.Ino != actual.Ino {
		return stat, fmt.Errorf("cortex: evidence database path changed")
	}
	return stat, nil
}

func checkFiles(name string, file *os.File, owner uint32) (bool, error) {
	stat, err := checkIdentity(name, file, owner)
	if err != nil {
		return false, err
	}
	empty := stat.Size == 0
	if !empty {
		var header [100]byte
		if _, err = file.ReadAt(header[:], 0); err != nil {
			return false, err
		}
		if string(header[:16]) != "SQLite format 3\x00" || binary.BigEndian.Uint32(header[68:72]) != applicationID || binary.BigEndian.Uint32(header[60:64]) != formatVersion {
			return false, fmt.Errorf("cortex: incompatible evidence database identity")
		}
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		_, sideErr := hostfs.StatPrivateState(name+suffix, owner)
		if errors.Is(sideErr, os.ErrNotExist) {
			continue
		}
		if sideErr != nil {
			return false, sideErr
		}
		if empty {
			return false, fmt.Errorf("cortex: empty evidence database has a sidecar")
		}
	}
	return empty, nil
}

func dataSource(name string) string {
	query := url.Values{"mode": {"rw"}, "_pragma": {"busy_timeout(5000)", "locking_mode(EXCLUSIVE)", "synchronous(FULL)", "foreign_keys(ON)", "trusted_schema(OFF)"}}
	return (&url.URL{Scheme: "file", Path: name, RawQuery: query.Encode()}).String()
}

func initialize(ctx context.Context, name string) (err error) {
	db, err := sql.Open("sqlite", dataSource(name))
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, db.Close()) }()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, statement := range []string{fmt.Sprintf("PRAGMA application_id=%d", applicationID), fmt.Sprintf("PRAGMA user_version=%d", formatVersion)} {
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return errors.Join(err, tx.Rollback())
		}
	}
	return tx.Commit()
}

func connect(ctx context.Context, name string) (*sqlitestore.Store, error) {
	drv := sqlitedriver.New()
	if err := drv.Open(ctx, dataSource(name), driver.WithPoolSize(1)); err != nil {
		return nil, err
	}
	var application, version uint32
	var integrity string
	for _, query := range []struct {
		statement string
		target    any
	}{
		{"PRAGMA application_id", &application}, {"PRAGMA user_version", &version}, {"PRAGMA quick_check(1)", &integrity},
	} {
		if err := drv.QueryRow(ctx, query.statement).Scan(query.target); err != nil {
			return nil, errors.Join(err, drv.Close())
		}
	}
	if application != applicationID || version != formatVersion || integrity != "ok" {
		return nil, errors.Join(fmt.Errorf("cortex: invalid evidence database state"), drv.Close())
	}
	db, err := grove.Open(drv)
	if err != nil {
		return nil, errors.Join(err, drv.Close())
	}
	data := sqlitestore.New(db)
	if err = data.Migrate(ctx); err != nil {
		return nil, errors.Join(err, data.Close())
	}
	return data, nil
}

func (s *Store) operation(ctx context.Context, fn func(*sqlitestore.Store) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.data == nil {
		return fmt.Errorf("cortex: evidence database: %w", os.ErrClosed)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(s.data)
}

// SaveEvidence preserves the root store's immutable write and tombstone fence.
func (s *Store) SaveEvidence(ctx context.Context, a evidence.Artifact) error {
	return s.operation(ctx, func(data *sqlitestore.Store) error { return data.SaveEvidence(ctx, a) })
}

// LoadEvidence reads an exact scoped binding through the root store.
func (s *Store) LoadEvidence(ctx context.Context, ref evidence.Ref) (a evidence.Artifact, err error) {
	err = s.operation(ctx, func(data *sqlitestore.Store) error { a, err = data.LoadEvidence(ctx, ref); return err })
	return a, err
}

// TombstoneEvidence deletes bytes and durably reserves the evidence identity.
func (s *Store) TombstoneEvidence(ctx context.Context, ref evidence.Ref, reason string) error {
	return s.operation(ctx, func(data *sqlitestore.Store) error { return data.TombstoneEvidence(ctx, ref, reason) })
}

// GetEvidenceTombstone returns raw metadata for the service's audit boundary.
func (s *Store) GetEvidenceTombstone(ctx context.Context, ref evidence.Ref) (proof evidence.Tombstone, err error) {
	err = s.operation(ctx, func(data *sqlitestore.Store) error { proof, err = data.GetEvidenceTombstone(ctx, ref); return err })
	return proof, err
}

// Close closes SQLite before releasing ownership and is safe to repeat.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data != nil {
		s.closeErr = errors.Join(s.data.Close(), s.file.Close(), s.lock.Close())
		s.data = nil
	}
	return s.closeErr
}
