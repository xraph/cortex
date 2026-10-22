//go:build linux || darwin

package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"golang.org/x/sys/unix"
)

// The protected lock file records identity before serving. A truncated record
// after a crash is not recovery authority. Keep the lock inode across lifetimes.
type controlSocketRecord struct {
	SchemaVersion int    `json:"schema_version"`
	Path          string `json:"path"`
	Device        string `json:"device"`
	Inode         uint64 `json:"inode"`
	Owner         uint32 `json:"owner"`
	Group         uint32 `json:"group"`
	Mode          uint64 `json:"mode"`
	Links         uint64 `json:"links"`
	ChangeSeconds int64  `json:"change_seconds"`
	ChangeNanos   int64  `json:"change_nanos"`
}

func newControlSocketRecord(path string, s unix.Stat_t) controlSocketRecord {
	return controlSocketRecord{SchemaVersion: 1, Path: path, Device: fmt.Sprint(s.Dev), Inode: s.Ino, Owner: s.Uid, Group: s.Gid, Mode: controlStatValue(s.Mode), Links: controlStatValue(s.Nlink), ChangeSeconds: s.Ctim.Sec, ChangeNanos: s.Ctim.Nsec}
}

func (e *controlEndpoint) recoverSocket(ctx context.Context, path string, owner uint32) error {
	body, err := io.ReadAll(io.LimitReader(e.lock, 4097))
	if err != nil {
		return err
	}
	var record controlSocketRecord
	if len(body) > 0 && (len(body) > 4096 || decodeCanonical(body, &record) != nil || record.SchemaVersion != 1 || record.Path != path || record.Owner != owner || record.Mode != unix.S_IFSOCK|0o600 || record.Links != 1 || record.Inode == 0 || record.ChangeNanos < 0 || record.ChangeNanos >= int64(time.Second)) {
		return fmt.Errorf("cortex: invalid control socket recovery record")
	}
	var current unix.Stat_t
	err = unix.Fstatat(int(e.directory.Fd()), e.name, &current, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(err, unix.ENOENT) {
		return ctx.Err()
	}
	if err != nil {
		return err
	}
	if len(body) == 0 || record != newControlSocketRecord(path, current) {
		return fmt.Errorf("cortex: unowned control socket; recovery refused")
	}
	// A lock alone cannot prove that no process retained the listener. Connect
	// without sending a request; only an explicit kernel refusal permits removal.
	probe, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	connection, dialErr := (&net.Dialer{}).DialContext(probe, "unix", path)
	if connection != nil {
		return errors.Join(fmt.Errorf("cortex: control socket is still listening"), connection.Close())
	}
	if !errors.Is(dialErr, unix.ECONNREFUSED) || probe.Err() != nil {
		return errors.Join(fmt.Errorf("cortex: control socket liveness is unresolved"), dialErr, probe.Err())
	}
	if operationErr := unix.Fstatat(int(e.directory.Fd()), e.name, &current, unix.AT_SYMLINK_NOFOLLOW); operationErr != nil {
		return operationErr
	}
	if record != newControlSocketRecord(path, current) {
		return fmt.Errorf("cortex: control socket changed during recovery")
	}
	if operationErr := ctx.Err(); operationErr != nil {
		return operationErr
	}
	if operationErr := unix.Unlinkat(int(e.directory.Fd()), e.name, 0); operationErr != nil {
		return operationErr
	}
	return e.directory.Sync()
}

func (e *controlEndpoint) recordSocket(path string) error {
	var current unix.Stat_t
	if err := unix.Fstatat(int(e.directory.Fd()), e.name, &current, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if !sameControlInode(e.socket, current) || current.Mode&0o7777 != 0o600 || current.Nlink != 1 {
		return fmt.Errorf("cortex: control socket changed before persistence")
	}
	record := newControlSocketRecord(path, current)
	body, err := json.Marshal(record)
	if err != nil {
		return err
	}
	n, err := e.lock.WriteAt(body, 0)
	if err != nil {
		return err
	}
	if n != len(body) {
		return io.ErrShortWrite
	}
	if operationErr := e.lock.Truncate(int64(len(body))); operationErr != nil {
		return operationErr
	}
	if operationErr := e.lock.Sync(); operationErr != nil {
		return operationErr
	}
	return e.directory.Sync()
}

// Stat mode and link widths vary across the supported operating systems and
// architectures. Both fields widen without narrowing or signed conversion.
func controlStatValue[T ~uint16 | ~uint32 | ~uint64](value T) uint64 { return uint64(value) }
