//go:build linux || darwin

package hostfs

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/evidence"
)

// PublishPin durably installs a mode-0400 snapshot in a private directory without
// replacing an existing name. An identical verified file is an idempotent retry.
// beforePublish must recheck current authority; it also runs for retries. An
// error after rename may leave the complete file installed. Retry with the same
// pin to resolve that ambiguity. A process crash may leave a private temporary
// file, never a partial destination. The trusted owner must protect paths during
// use. Filesystems without exclusive rename support fail without a fallback.
func PublishPin(ctx context.Context, pin Pin, owner uint32, data []byte, beforePublish func() error) (err error) {
	if beforePublish == nil || pin.Mode != 0o400 || pin.Bytes < 0 || pin.Bytes > 64<<20 || int64(len(data)) != pin.Bytes {
		return fmt.Errorf("cortex: invalid snapshot publication")
	}
	data = bytes.Clone(data)
	if pin.SHA256 != evidence.Digest(data) {
		return evidence.ErrDigestMismatch
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return contextErr
	}
	directory, err := ownedParent(pin.Path, owner, pin.Mode, true)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, directory.Close()) }()
	authorize := func() error {
		if contextErr := ctx.Err(); contextErr != nil {
			return contextErr
		}
		if policyErr := beforePublish(); policyErr != nil {
			return policyErr
		}
		return ctx.Err()
	}
	if existingErr := VerifyPin(ctx, pin, owner); existingErr == nil {
		if policyErr := authorize(); policyErr != nil {
			return policyErr
		}
		return errors.Join(VerifyPin(ctx, pin, owner), directory.Sync(), ctx.Err())
	} else if !errors.Is(existingErr, os.ErrNotExist) {
		return existingErr
	}

	name := ".cortex-publish-" + rand.Text() + ".tmp"
	fd, err := unix.Openat(int(directory.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	temporary := true
	defer func() {
		if temporary {
			cleanupErr := unix.Unlinkat(int(directory.Fd()), name, 0)
			err = errors.Join(err, cleanupErr, directory.Sync())
		}
	}()
	file := os.NewFile(uintptr(fd), name)
	written, writeErr := file.Write(data)
	if writeErr == nil && written != len(data) {
		writeErr = io.ErrShortWrite
	}
	if writeErr == nil {
		writeErr = file.Chmod(0o400)
	}
	if writeErr == nil {
		writeErr = file.Sync()
	}
	if operationErr := errors.Join(writeErr, file.Close(), ctx.Err()); operationErr != nil {
		return operationErr
	}
	if policyErr := authorize(); policyErr != nil {
		return policyErr
	}
	if renameErr := renameExclusive(int(directory.Fd()), name, filepath.Base(pin.Path)); renameErr != nil {
		if !errors.Is(renameErr, unix.EEXIST) {
			return renameErr
		}
		// Another publisher won. Its file must independently match the same pin.
		return errors.Join(VerifyPin(ctx, pin, owner), ctx.Err())
	}
	temporary = false
	if syncErr := directory.Sync(); syncErr != nil {
		return syncErr
	}
	return errors.Join(VerifyPin(ctx, pin, owner), ctx.Err())
}
