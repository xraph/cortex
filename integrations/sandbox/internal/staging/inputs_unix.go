//go:build linux || darwin

// Package staging prepares verified evidence files without granting execution.
package staging

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex"
	"github.com/xraph/cortex/evidence"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/sandbox"
)

// Source is the read capability of evidence.Repository. Open must honor context;
// closing its returned reader must unblock an outstanding read.
type Source interface {
	Open(context.Context, evidence.Ref) (io.ReadCloser, error)
}

// Config is trusted worker configuration. AuthorizeJob must bind the host's
// authenticated context principal to the job and recheck its current grant.
// Policy checks evidence access independently. Root must be private and owned
// by OwnerUID (zero for the production Linux worker). Limits cannot exceed one
// GiB per input or four GiB total. Now is an optional trusted host clock.
type Config struct {
	Root                      string
	OwnerUID                  uint32
	PerInputBytes, TotalBytes int64
	Source                    Source
	Policy                    evidence.Authorizer
	AuthorizeJob              func(context.Context, sandbox.Job) error
	Now                       func() time.Time
}

// Inputs maps guest paths to verified read-only files in a completed private
// staging directory. The host must reauthorize before dispatch. This value does
// not claim a job, verify its image or make its bundle executable.
type Inputs struct {
	JobDigest string
	Directory string
	Paths     map[string]string
}

type completion struct {
	SchemaVersion int    `json:"schema_version"`
	JobDigest     string `json:"job_digest"`
}

// Stage verifies copies before publishing a durable completion record. Retries
// verify existing bytes and reopen the source to catch deletion or revocation.
// Interrupted directories are preserved for an authorized, fenced reaper;
// Stage never resets or overwrites an existing job directory.
func Stage(ctx context.Context, cfg Config, job sandbox.Job) (result *Inputs, err error) {
	if int64(cfg.OwnerUID) != int64(os.Geteuid()) {
		return nil, fmt.Errorf("cortex: staging owner must match worker identity")
	}
	if cfg.Source == nil || cfg.Policy == nil || cfg.AuthorizeJob == nil || cfg.PerInputBytes <= 0 || cfg.PerInputBytes > 1<<30 || cfg.TotalBytes <= 0 || cfg.TotalBytes > 4<<30 {
		return nil, fmt.Errorf("cortex: invalid input staging configuration")
	}
	if operationErr := job.Validate(); operationErr != nil {
		return nil, operationErr
	}
	job, err = sandbox.Seal(job)
	if err != nil {
		return nil, err
	}
	scope, err := cortex.RequireScope(ctx)
	if err != nil {
		return nil, err
	}
	if scope.Canonical() != job.Scope.Canonical() {
		return nil, evidence.ErrDenied
	}
	remaining := cfg.TotalBytes
	for _, input := range job.Inputs {
		if input.Ref.Bytes > cfg.PerInputBytes || input.Ref.Bytes > remaining {
			return nil, evidence.ErrTooLarge
		}
		remaining -= input.Ref.Bytes
	}
	ctx, cancel := context.WithTimeout(ctx, job.Limits.WallTime)
	defer cancel()
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if operationErr := authorize(ctx, cfg, job); operationErr != nil {
		return nil, operationErr
	}
	root, err := hostfs.OpenDirectory(cfg.Root, cfg.OwnerUID)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	info, err := root.Stat()
	if err != nil {
		return nil, err
	}
	if info.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("cortex: input staging root must be private")
	}
	name := evidence.Digest([]byte(job.Scope.Canonical())) + "-" + job.ID.String()
	fresh := true
	if err = unix.Mkdirat(int(root.Fd()), name, 0o700); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return nil, err
		}
		fresh = false
	}
	fd, err := unix.Openat(int(root.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	directory := os.NewFile(uintptr(fd), name)
	defer directory.Close()
	var stat unix.Stat_t
	if operationErr := unix.Fstat(fd, &stat); operationErr != nil {
		return nil, operationErr
	}
	if stat.Uid != cfg.OwnerUID || stat.Mode&0o7777 != 0o700 {
		return nil, fmt.Errorf("cortex: unsafe input staging directory")
	}
	created := make([]string, 0, len(job.Inputs)+2)
	defer func() {
		if fresh && err != nil {
			err = errors.Join(err, cleanup(root, directory, name, created))
		}
	}()
	if fresh {
		for i, input := range job.Inputs {
			fileName := inputName(i)
			var file *os.File
			file, err = createFile(directory, fileName)
			if err != nil {
				return nil, err
			}
			created = append(created, fileName)
			copyErr := copySource(ctx, cfg, input.Ref, file)
			if copyErr == nil {
				copyErr = file.Chmod(0o444)
			}
			if copyErr == nil {
				copyErr = file.Sync()
			}
			err = errors.Join(copyErr, file.Close())
			if err != nil {
				return nil, err
			}
		}
		if operationErr := authorize(ctx, cfg, job); operationErr != nil {
			return nil, operationErr
		}
		body, encodeErr := json.Marshal(completion{SchemaVersion: 1, JobDigest: job.Digest})
		if encodeErr != nil {
			return nil, encodeErr
		}
		file, createErr := createFile(directory, "manifest.tmp")
		if createErr != nil {
			return nil, createErr
		}
		created = append(created, "manifest.tmp")
		_, writeErr := file.Write(body)
		if writeErr == nil {
			writeErr = file.Chmod(0o400)
		}
		if writeErr == nil {
			writeErr = file.Sync()
		}
		if operationErr := errors.Join(writeErr, file.Close()); operationErr != nil {
			return nil, operationErr
		}
		if operationErr := unix.Renameat(fd, "manifest.tmp", fd, "manifest.json"); operationErr != nil {
			return nil, operationErr
		}
		created = append(created, "manifest.json")
		if operationErr := directory.Sync(); operationErr != nil {
			return nil, operationErr
		}
		if operationErr := root.Sync(); operationErr != nil {
			return nil, operationErr
		}
	}
	return verifyExisting(ctx, cfg, job, directory, filepath.Join(cfg.Root, name), !fresh)
}

func authorize(ctx context.Context, cfg Config, job sandbox.Job) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	copyJob, err := sandbox.Seal(job)
	if err != nil {
		return err
	}
	if operationErr := cfg.AuthorizeJob(ctx, copyJob); operationErr != nil {
		return operationErr
	}
	for _, input := range job.Inputs {
		now := cfg.Now()
		if now.IsZero() || now.Before(input.Ref.CreatedAt) || !now.Before(input.Ref.RetainUntil) {
			return evidence.ErrDenied
		}
		if operationErr := cfg.Policy.AuthorizeEvidence(ctx, evidence.ActionRead, cloneRef(input.Ref)); operationErr != nil {
			return operationErr
		}
		if operationErr := ctx.Err(); operationErr != nil {
			return operationErr
		}
	}
	return ctx.Err()
}

func cloneRef(ref evidence.Ref) evidence.Ref {
	ref.Scope.Levels = append([]cortex.Level(nil), ref.Scope.Levels...)
	return ref
}
func inputName(index int) string { return fmt.Sprintf("input-%03d", index) }

func createFile(directory *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), name), nil
}

func copySource(ctx context.Context, cfg Config, ref evidence.Ref, destination io.Writer) error {
	reader, err := cfg.Source.Open(ctx, cloneRef(ref))
	if err != nil {
		if reader != nil {
			err = errors.Join(err, reader.Close())
		}
		return err
	}
	if reader == nil {
		return fmt.Errorf("cortex: evidence source returned no reader")
	}
	var once sync.Once
	var closeErr error
	closeReader := func() { once.Do(func() { closeErr = reader.Close() }) }
	stop := context.AfterFunc(ctx, closeReader)
	copyErr := copyVerified(ctx, reader, destination, ref)
	stop()
	closeReader()
	return errors.Join(copyErr, closeErr, ctx.Err())
}

func copyVerified(ctx context.Context, source io.Reader, destination io.Writer, ref evidence.Ref) error {
	reader := io.LimitReader(source, ref.Bytes+1)
	hash := sha256.New()
	writer := io.MultiWriter(destination, hash)
	buffer := make([]byte, 64<<10)
	var total int64
	empty := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			total += int64(n)
			if total > ref.Bytes {
				return evidence.ErrDigestMismatch
			}
			written, writeErr := writer.Write(buffer[:n])
			if writeErr != nil {
				return writeErr
			}
			if written != n {
				return io.ErrShortWrite
			}
			empty = 0
		} else {
			empty++
			if empty > 100 {
				return io.ErrNoProgress
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if total != ref.Bytes || hex.EncodeToString(hash.Sum(nil)) != ref.SHA256 {
		return evidence.ErrDigestMismatch
	}
	return ctx.Err()
}

func openFile(directory *os.File, name string, owner uint32, mode uint64, maximum int64) (*os.File, error) {
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	var stat unix.Stat_t
	if err = unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != owner || uint64(stat.Mode)&0o7777 != mode || stat.Size > maximum {
		file.Close()
		return nil, fmt.Errorf("cortex: unsafe staged input file")
	}
	return file, nil
}

func verifyExisting(ctx context.Context, cfg Config, job sandbox.Job, directory *os.File, hostPath string, recheckSource bool) (*Inputs, error) {
	manifest, err := openFile(directory, "manifest.json", cfg.OwnerUID, 0o400, 512)
	if errors.Is(err, os.ErrNotExist) {
		return nil, cortex.ErrSandboxInputsIncomplete
	}
	if err != nil {
		return nil, err
	}
	body, readErr := io.ReadAll(io.LimitReader(manifest, 513))
	if operationErr := errors.Join(readErr, manifest.Close()); operationErr != nil {
		return nil, operationErr
	}
	want, err := json.Marshal(completion{SchemaVersion: 1, JobDigest: job.Digest})
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(body, want) {
		return nil, cortex.ErrSandboxInputsConflict
	}
	entries, err := directory.ReadDir(len(job.Inputs) + 2)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) != len(job.Inputs)+1 {
		return nil, fmt.Errorf("cortex: staged input inventory mismatch")
	}
	paths := make(map[string]string, len(job.Inputs))
	for i, input := range job.Inputs {
		name := inputName(i)
		file, openErr := openFile(directory, name, cfg.OwnerUID, 0o444, input.Ref.Bytes)
		if openErr != nil {
			return nil, openErr
		}
		if operationErr := errors.Join(copyVerified(ctx, file, io.Discard, input.Ref), file.Close()); operationErr != nil {
			return nil, operationErr
		}
		if recheckSource {
			if sourceErr := copySource(ctx, cfg, input.Ref, io.Discard); sourceErr != nil {
				return nil, sourceErr
			}
		}
		paths[input.Path] = filepath.Join(hostPath, name)
	}
	if operationErr := authorize(ctx, cfg, job); operationErr != nil {
		return nil, operationErr
	}
	return &Inputs{JobDigest: job.Digest, Directory: hostPath, Paths: paths}, nil
}

func cleanup(root, directory *os.File, name string, created []string) error {
	var failures []error
	for _, file := range created {
		if err := unix.Unlinkat(int(directory.Fd()), file, 0); err != nil && !errors.Is(err, unix.ENOENT) {
			failures = append(failures, err)
		}
	}
	if err := unix.Unlinkat(int(root.Fd()), name, unix.AT_REMOVEDIR); err != nil {
		failures = append(failures, err)
	}
	return errors.Join(append(failures, root.Sync())...)
}
