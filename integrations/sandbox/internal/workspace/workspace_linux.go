//go:build linux

package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/containment"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/sandbox"
)

// Config binds workspace storage to one signed job and its original deadline.
// Root must already exist with root ownership and mode 0o700. Authorize must check
// current host grants for this exact job and honor context cancellation.
type Config struct {
	Root          string
	Policy        Policy
	Job           sandbox.Job
	ServiceMemory uint64
	Deadline      time.Time
	Authorize     func(context.Context, sandbox.Job) error
}

// Paths names host-controlled staging and guest scratch directories. They carry
// no authority; call Check before using them for staging, execution or export.
type Paths struct{ Directory, Images, Inputs, Bundle, Runtime, Scratch string }

// Workspace owns one exclusive job directory and two quota-enforced filesystems.
// It never reuses retained job directories. Failed cleanup requires retry or
// independent service cleanup before the retained directory can be reclaimed.
type Workspace struct {
	mu                     sync.Mutex
	cfg                    Config
	verify                 func(context.Context, id.ID) error
	root, directory        *os.File
	paths                  Paths
	children               []ownedDirectory
	mounts                 [2]*mountedTmpfs
	ready, closing, closed bool
}

type ownedDirectory struct {
	name string
	file *os.File
}

// Create requires the exact Linux worker containment and a private mount
// namespace. A nonnil workspace returned with an error has unresolved cleanup;
// it cannot be used and the caller must retain it for Close or exit the worker.
func Create(ctx context.Context, cfg Config) (*Workspace, error) {
	return create(ctx, cfg, containment.VerifyCurrent)
}

func create(ctx context.Context, cfg Config, verify func(context.Context, id.ID) error) (workspace *Workspace, resultErr error) {
	if verify == nil || cfg.Authorize == nil || cfg.Deadline.IsZero() || cfg.Job.Validate() != nil || cortex.ScopeFromContext(ctx).Canonical() != cfg.Job.Scope.Canonical() {
		return nil, fmt.Errorf("cortex: invalid workspace authority or scope")
	}
	job, err := sandbox.Seal(cfg.Job)
	if err != nil {
		return nil, err
	}
	cfg.Job = job
	stage, scratch, err := cfg.Policy.mountPlans(job, cfg.ServiceMemory, int64(os.Getpagesize()))
	if err != nil {
		return nil, err
	}
	operation, cancel := context.WithDeadline(ctx, cfg.Deadline)
	defer cancel()
	w := &Workspace{cfg: cfg, verify: verify}
	if checkErr := w.authorize(operation); checkErr != nil {
		return nil, checkErr
	}
	// Native mount setup independently checks namespace and ownership. Verify it
	// before even creating the outer directory, so ambient root leaves no files.
	if _, err = privateNamespace(); err != nil {
		return nil, err
	}
	w.root, err = hostfs.OpenDirectory(cfg.Root, 0)
	if err != nil {
		return nil, err
	}
	if err = privateDirectory(w.root); err != nil {
		w.root.Close()
		return nil, err
	}
	name := job.ID.String()
	if err = unix.Mkdirat(int(w.root.Fd()), name, 0o700); err != nil {
		w.root.Close()
		return nil, err
	}
	// From this point every failure attempts cleanup with an independent budget.
	defer func() {
		if resultErr == nil {
			return
		}
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		cleanupErr := w.Close(cleanup)
		resultErr = errors.Join(resultErr, cleanupErr)
		if cleanupErr == nil {
			workspace = nil
		} else {
			workspace = w
		}
	}()
	fd, err := unix.Openat(int(w.root.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return w, err
	}
	w.directory = os.NewFile(uintptr(fd), name)
	base := filepath.Join(cfg.Root, name)
	w.paths = Paths{Directory: base, Images: filepath.Join(base, "stage", "images"), Inputs: filepath.Join(base, "stage", "inputs"), Bundle: filepath.Join(base, "stage", "bundle"), Runtime: filepath.Join(base, "stage", "runtime"), Scratch: filepath.Join(base, "scratch")}
	for i, child := range []string{"stage", "scratch"} {
		if checkErr := unix.Mkdirat(fd, child, 0o700); checkErr != nil {
			return w, checkErr
		}
		childFD, openErr := unix.Openat(fd, child, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if openErr != nil {
			return w, openErr
		}
		w.children = append(w.children, ownedDirectory{name: child, file: os.NewFile(uintptr(childFD), child)})
		plan := stage
		if i == 1 {
			plan = scratch
		}
		w.mounts[i], err = mountTmpfs(operation, fd, child, plan)
		if err != nil {
			return w, err
		}
	}
	stageFD, err := unix.Openat(fd, "stage", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return w, err
	}
	stageRoot := os.NewFile(uintptr(stageFD), "stage")
	for _, child := range []string{"images", "inputs", "bundle", "runtime"} {
		if err = unix.Mkdirat(stageFD, child, 0o700); err != nil {
			stageRoot.Close()
			return w, err
		}
	}
	if checkErr := stageRoot.Close(); checkErr != nil {
		return w, checkErr
	}
	if checkErr := w.authorize(operation); checkErr != nil {
		return w, checkErr
	}
	if checkErr := w.checkStorage(operation); checkErr != nil {
		return w, checkErr
	}
	w.ready = true
	return w, nil
}

func (w *Workspace) authorize(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := w.verify(ctx, w.cfg.Job.ID); err != nil {
		return err
	}
	job, err := sandbox.Seal(w.cfg.Job)
	if err != nil {
		return err
	}
	// Scope and mutable job data are detached from the retained signed state.
	if checkErr := w.cfg.Authorize(cortex.WithScope(ctx, job.Scope), job); checkErr != nil {
		return checkErr
	}
	if checkErr := w.verify(ctx, w.cfg.Job.ID); checkErr != nil {
		return checkErr
	}
	return ctx.Err()
}

// Paths returns a snapshot only while setup is complete and cleanup has not begun.
func (w *Workspace) Paths() (Paths, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.ready || w.closing || w.closed {
		return Paths{}, fmt.Errorf("cortex: workspace is unavailable")
	}
	return w.paths, nil
}

// Check revalidates current authority, containment and storage without extending
// the signed deadline. It serializes with Close so cleanup cannot overlap it.
func (w *Workspace) Check(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.ready || w.closing || w.closed || cortex.ScopeFromContext(ctx).Canonical() != w.cfg.Job.Scope.Canonical() {
		return fmt.Errorf("cortex: workspace is unavailable or out of scope")
	}
	operation, cancel := context.WithDeadline(ctx, w.cfg.Deadline)
	defer cancel()
	if err := w.authorize(operation); err != nil {
		return err
	}
	return w.checkStorage(operation)
}

func (w *Workspace) checkStorage(ctx context.Context) error {
	if err := w.checkDirectory(); err != nil {
		return err
	}
	current, err := hostfs.OpenDirectory(w.cfg.Root, 0)
	if err != nil {
		return err
	}
	defer current.Close()
	if checkErr := sameDirectory(w.root, current); checkErr != nil {
		return checkErr
	}
	for _, mount := range w.mounts {
		if mount == nil {
			return fmt.Errorf("cortex: workspace filesystem missing")
		}
		if checkErr := mount.check(ctx); checkErr != nil {
			return checkErr
		}
	}
	return ctx.Err()
}

// Close retries normal unmount and removes only owned, empty mountpoints and the
// original job directory. It ignores execution grants and uses a fresh caller
// context capped at ten seconds; cancellation of the job does not waive cleanup.
func (w *Workspace) Close(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	w.ready = false
	w.closing = true
	operation, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := operation.Err(); err != nil {
		return err
	}
	if err := w.checkDirectory(); err != nil {
		return err
	}
	var cleanupErr error
	for i := len(w.mounts) - 1; i >= 0; i-- {
		if w.mounts[i] == nil {
			continue
		}
		if err := w.mounts[i].close(operation); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		} else {
			w.mounts[i] = nil
		}
	}
	if cleanupErr != nil {
		return cleanupErr
	}
	for len(w.children) > 0 {
		if err := operation.Err(); err != nil {
			return err
		}
		index := len(w.children) - 1
		child := w.children[index]
		fd, err := unix.Openat(int(w.directory.Fd()), child.name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return err
		}
		current := os.NewFile(uintptr(fd), child.name)
		identityErr := sameDirectory(child.file, current)
		closeErr := current.Close()
		if checkErr := errors.Join(identityErr, closeErr); checkErr != nil {
			return checkErr
		}
		if checkErr := unix.Unlinkat(int(w.directory.Fd()), child.name, unix.AT_REMOVEDIR); checkErr != nil {
			return checkErr
		}
		w.children = w.children[:index]
		if checkErr := child.file.Close(); checkErr != nil {
			return checkErr
		}
	}
	if err := w.checkDirectory(); err != nil {
		return err
	}
	if err := operation.Err(); err != nil {
		return err
	}
	if err := unix.Unlinkat(int(w.root.Fd()), w.cfg.Job.ID.String(), unix.AT_REMOVEDIR); err != nil {
		return err
	}
	w.closed = true
	return errors.Join(w.directory.Close(), w.root.Close())
}

func (w *Workspace) checkDirectory() error {
	if w.root == nil || w.directory == nil {
		return fmt.Errorf("cortex: workspace directory ownership unresolved")
	}
	if err := privateDirectory(w.root); err != nil {
		return err
	}
	if err := privateDirectory(w.directory); err != nil {
		return err
	}
	fd, err := unix.Openat(int(w.root.Fd()), w.cfg.Job.ID.String(), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	current := os.NewFile(uintptr(fd), "current workspace")
	defer current.Close()
	return sameDirectory(w.directory, current)
}

func privateDirectory(file *os.File) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Uid != 0 || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o7777 != 0o700 {
		return fmt.Errorf("cortex: workspace directory must be private and root owned")
	}
	return nil
}

func sameDirectory(left, right *os.File) error {
	var first, second unix.Stat_t
	if err := unix.Fstat(int(left.Fd()), &first); err != nil {
		return err
	}
	if err := unix.Fstat(int(right.Fd()), &second); err != nil {
		return err
	}
	if first.Dev != second.Dev || first.Ino != second.Ino {
		return fmt.Errorf("cortex: workspace directory was replaced")
	}
	return nil
}
