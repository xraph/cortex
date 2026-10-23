//go:build linux || darwin

package evidencedb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/id"
	sqlitestore "github.com/xraph/cortex/store/sqlite"
)

func databasePath(t *testing.T) string {
	t.Helper()
	directory, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(directory, "evidence.db")
}
func testArtifact(t *testing.T) evidence.Artifact {
	t.Helper()
	now := time.Now().UTC()
	artifact, err := evidence.Seal(evidence.Artifact{Ref: evidence.Ref{SchemaVersion: 1, ID: id.New(evidence.PrefixArtifact), Scope: cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "one"}}}, MediaType: "text/plain", Version: "source:1", Producer: "fixture", Sensitivity: evidence.SensitivityRestricted, CreatedAt: now, RetainUntil: now.Add(time.Hour)}, Data: []byte("retained evidence")})
	if err != nil {
		t.Fatal(err)
	}
	return artifact
}
func openFixture(t *testing.T, name string) *Store {
	t.Helper()
	s, err := open(t.Context(), name, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestEvidenceDatabasePersistsContentAndDeletionAcrossRestart(t *testing.T) {
	name := databasePath(t)
	artifact := testArtifact(t)
	ctx := cortex.WithScope(t.Context(), artifact.Ref.Scope)
	s := openFixture(t, name)
	if err := s.SaveEvidence(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openFixture(t, name)
	actual, err := s.LoadEvidence(ctx, artifact.Ref)
	if err != nil || !bytes.Equal(actual.Data, artifact.Data) {
		t.Fatal(actual, err)
	}
	ctx = evidence.WithDeletionActor(ctx, "service:cleanup")
	if err = s.TombstoneEvidence(ctx, artifact.Ref, "abandoned"); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openFixture(t, name)
	proof, err := s.GetEvidenceTombstone(ctx, artifact.Ref)
	if err != nil || proof.Actor != "service:cleanup" || proof.Reason != "abandoned" {
		t.Fatal(proof, err)
	}
	if err = s.SaveEvidence(ctx, artifact); !errors.Is(err, evidence.ErrDeleted) {
		t.Fatal("resurrected content", err)
	}
	if _, err = s.LoadEvidence(ctx, artifact.Ref); !errors.Is(err, evidence.ErrDeleted) {
		t.Fatal("read deleted content", err)
	}
}

func TestEvidenceDatabaseRejectsConcurrentOwnershipAndClosedUse(t *testing.T) {
	name := databasePath(t)
	s := openFixture(t, name)
	if other, err := open(t.Context(), name, uint32(os.Geteuid())); err == nil || other != nil {
		t.Fatal("duplicate owner", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal("non-idempotent close", err)
	}
	artifact := testArtifact(t)
	ctx := cortex.WithScope(t.Context(), artifact.Ref.Scope)
	if err := s.SaveEvidence(ctx, artifact); err == nil {
		t.Fatal("closed write accepted")
	}
	reopened := openFixture(t, name)
	if err := reopened.SaveEvidence(ctx, artifact); err != nil {
		t.Fatal("ownership not released", err)
	}
}

func TestEvidenceDatabaseRejectsUnsafeFilesBeforeSQLiteAccess(t *testing.T) {
	for _, mode := range []string{"parent", "main-mode", "main-link", "main-hardlink", "main-fifo", "wal-link", "shm-mode", "journal-hardlink", "lock-mode", "lock-link", "lock-content", "owner", "relative", "parent-alias", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			name := databasePath(t)
			s := openFixture(t, name)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			owner := uint32(os.Geteuid())
			ctx := t.Context()
			original := name
			switch mode {
			case "parent":
				err = os.Chmod(filepath.Dir(name), 0o750)
			case "main-mode":
				err = os.Chmod(name, 0o644)
			case "main-link":
				err = os.Symlink(name, name+".alias")
				name += ".alias"
			case "main-hardlink":
				err = os.Link(name, name+".alias")
			case "main-fifo":
				name += ".fifo"
				err = unix.Mkfifo(name, 0o600)
			case "wal-link":
				err = os.Symlink(name, name+"-wal")
			case "shm-mode":
				err = os.WriteFile(name+"-shm", []byte("unchanged"), 0o644)
			case "journal-hardlink":
				err = os.Link(name, name+"-journal")
			case "lock-mode":
				err = os.Chmod(name+".lock", 0o644)
			case "lock-link":
				if err = os.Remove(name + ".lock"); err == nil {
					err = os.Symlink(name, name+".lock")
				}
			case "lock-content":
				err = os.WriteFile(name+".lock", []byte("foreign"), 0o600)
			case "owner":
				owner++
			case "relative":
				name = "evidence.db"
			case "parent-alias":
				err = os.Symlink(filepath.Dir(name), name+".dir")
				name = filepath.Join(name+".dir", "evidence.db")
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if err != nil {
				t.Fatal(err)
			}
			if bad, openErr := open(ctx, name, owner); openErr == nil || bad != nil {
				t.Fatal("unsafe database accepted", openErr)
			}
			after, err := os.ReadFile(original)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("rejected file changed", err)
			}
		})
	}
}

func TestEvidenceDatabaseRejectsForeignDatabaseWithoutMutation(t *testing.T) {
	for _, mode := range []string{"plain", "sqlite", "worker-role", "future-format", "evidence-corrupt"} {
		t.Run(mode, func(t *testing.T) {
			name := databasePath(t)
			if err := os.WriteFile(name, []byte{}, 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "plain" {
				if err := os.WriteFile(name, []byte("not a database"), 0o600); err != nil {
					t.Fatal(err)
				}
			} else {
				db, err := sql.Open("sqlite", name)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.ExecContext(t.Context(), "CREATE TABLE unrelated (value TEXT)"); err != nil {
					t.Fatal(err)
				}
				if mode == "worker-role" {
					if _, err = db.ExecContext(t.Context(), "PRAGMA application_id=1129862987"); err != nil {
						t.Fatal(err)
					}
				}
				if mode == "future-format" {
					if _, err = db.ExecContext(t.Context(), fmt.Sprintf("PRAGMA application_id=%d; PRAGMA user_version=2", applicationID)); err != nil {
						t.Fatal(err)
					}
				}
				if err = db.Close(); err != nil {
					t.Fatal(err)
				}
				if mode == "evidence-corrupt" {
					if err = os.Truncate(name, 31); err != nil {
						t.Fatal(err)
					}
				}
			}
			before, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if s, openErr := open(t.Context(), name, uint32(os.Geteuid())); openErr == nil || s != nil {
				t.Fatal("foreign database accepted", openErr)
			}
			after, err := os.ReadFile(name)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("foreign database modified", err)
			}
		})
	}
}

func TestEvidenceDatabaseRecoversAfterProcessExit(t *testing.T) {
	for _, mode := range []string{"stored", "deleted"} {
		t.Run(mode, func(t *testing.T) {
			name := databasePath(t)
			artifact := testArtifact(t)
			payload, err := json.Marshal(artifact)
			if err != nil {
				t.Fatal(err)
			}
			child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestEvidenceDatabaseCrashProcess$")
			child.Env = append(os.Environ(), "CORTEX_EVIDENCE_CRASH_PATH="+name, "CORTEX_EVIDENCE_CRASH_ARTIFACT="+string(payload), "CORTEX_EVIDENCE_CRASH_MODE="+mode)
			if output, runErr := child.CombinedOutput(); runErr != nil {
				t.Fatalf("child: %v %s", runErr, output)
			}
			wal, err := os.Stat(name + "-wal")
			if err != nil || wal.Size() == 0 {
				t.Fatal("fixture did not leave a WAL", err)
			}
			s := openFixture(t, name)
			ctx := cortex.WithScope(t.Context(), artifact.Ref.Scope)
			if mode == "stored" {
				actual, readErr := s.LoadEvidence(ctx, artifact.Ref)
				if readErr != nil || !bytes.Equal(actual.Data, artifact.Data) {
					t.Fatal("lost committed bytes", readErr)
				}
			} else {
				proof, readErr := s.GetEvidenceTombstone(ctx, artifact.Ref)
				if readErr != nil || proof.Actor != "service:crash-cleanup" {
					t.Fatal("lost deletion proof", readErr)
				}
				if saveErr := s.SaveEvidence(ctx, artifact); !errors.Is(saveErr, evidence.ErrDeleted) {
					t.Fatal("late upload after crash", saveErr)
				}
			}
		})
	}
}

func TestEvidenceDatabaseCrashProcess(t *testing.T) {
	name := os.Getenv("CORTEX_EVIDENCE_CRASH_PATH")
	if name == "" {
		return
	}
	if os.Getenv("CORTEX_EVIDENCE_CRASH_MODE") == "probe" {
		raw, openErr := sql.Open("sqlite", name)
		if openErr != nil {
			t.Fatal(openErr)
		}
		defer raw.Close()
		var tables int
		if readErr := raw.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sqlite_schema").Scan(&tables); readErr == nil {
			t.Fatal("another process read an owned database")
		}
		return
	}
	var artifact evidence.Artifact
	if err := json.Unmarshal([]byte(os.Getenv("CORTEX_EVIDENCE_CRASH_ARTIFACT")), &artifact); err != nil {
		t.Fatal(err)
	}
	s, err := open(t.Context(), name, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := cortex.WithScope(t.Context(), artifact.Ref.Scope)
	if err = s.SaveEvidence(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CORTEX_EVIDENCE_CRASH_MODE") == "deleted" {
		if err = s.TombstoneEvidence(evidence.WithDeletionActor(ctx, "service:crash-cleanup"), artifact.Ref, "abandoned"); err != nil {
			t.Fatal(err)
		}
	}
	// Exit without Close so SQLite cannot checkpoint the committed WAL.
	os.Exit(0)
}

func TestEvidenceDatabaseBoundsScopeAndEncodesFileNames(t *testing.T) {
	name := filepath.Join(filepath.Dir(databasePath(t)), "evidence ?#%.db")
	s := openFixture(t, name)
	artifact := testArtifact(t)
	if err := s.SaveEvidence(t.Context(), artifact); !errors.Is(err, cortex.ErrNoScope) {
		t.Fatal("missing scope", err)
	}
	ctx := cortex.WithScope(t.Context(), artifact.Ref.Scope)
	if err := s.SaveEvidence(ctx, artifact); err != nil {
		t.Fatal(err)
	}
	foreign := cortex.WithScope(t.Context(), cortex.Scope{Levels: []cortex.Level{{Key: "workspace", Value: "other"}}})
	if _, err := s.LoadEvidence(foreign, artifact.Ref); err == nil {
		t.Fatal("foreign scope read")
	}
	oversized, err := evidence.Seal(evidence.Artifact{Ref: artifact.Ref, Data: make([]byte, (1<<20)+1)})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveEvidence(ctx, oversized); !errors.Is(err, evidence.ErrTooLarge) {
		t.Fatal("storage byte bound", err)
	}
	if _, err = os.Stat(name); err != nil {
		t.Fatal("filename was interpreted as DSN", err)
	}
}

func TestEvidenceDatabaseRejectsEmptyFileWithSidecars(t *testing.T) {
	name := databasePath(t)
	if err := os.WriteFile(name, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name+"-wal", []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s, err := open(t.Context(), name, uint32(os.Geteuid())); err == nil || s != nil {
		t.Fatal("adopted orphan sidecar", err)
	}
	info, err := os.Stat(name)
	if err != nil || info.Size() != 0 {
		t.Fatal("empty file changed", err)
	}
}

func TestEvidenceDatabaseProductionRequiresRoot(t *testing.T) {
	s, err := Open(t.Context(), databasePath(t))
	if os.Geteuid() != 0 {
		if err == nil || s != nil {
			t.Fatal("production accepted non-root owner")
		}
	} else {
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestEvidenceDatabaseCloseRetainsOwnershipUntilOperationsFinish(t *testing.T) {
	name := databasePath(t)
	s := openFixture(t, name)
	artifact := testArtifact(t)
	ctx := cortex.WithScope(t.Context(), artifact.Ref.Scope)
	entered, release := make(chan struct{}), make(chan struct{})
	operationDone := make(chan error, 1)
	go func() {
		operationDone <- s.operation(ctx, func(data *sqlitestore.Store) error {
			close(entered)
			<-release
			return data.SaveEvidence(ctx, artifact)
		})
	}()
	<-entered
	closing := make(chan struct{})
	closed := make(chan error, 1)
	go func() { close(closing); closed <- s.Close() }()
	<-closing
	if other, err := open(t.Context(), name, uint32(os.Geteuid())); err == nil || other != nil {
		close(release)
		t.Fatal("ownership released before operation finished", err)
	}
	close(release)
	if err := <-operationDone; err != nil {
		t.Fatal(err)
	}
	if err := <-closed; err != nil {
		t.Fatal(err)
	}
	reopened := openFixture(t, name)
	if actual, err := reopened.LoadEvidence(ctx, artifact.Ref); err != nil || !bytes.Equal(actual.Data, artifact.Data) {
		t.Fatal("active operation lost", err)
	}
}

func TestEvidenceDatabaseExcludesOtherSQLiteProcesses(t *testing.T) {
	name := databasePath(t)
	openFixture(t, name)
	child := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestEvidenceDatabaseCrashProcess$")
	child.Env = append(os.Environ(), "CORTEX_EVIDENCE_CRASH_PATH="+name, "CORTEX_EVIDENCE_CRASH_MODE=probe")
	if output, err := child.CombinedOutput(); err != nil {
		t.Fatalf("SQLite ownership probe: %v %s", err, output)
	}
}
