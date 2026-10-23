//go:build linux

package retention

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/id"
	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/sandbox"
)

type retainedEntry struct {
	name string
	file *os.File
}

type recoveryIdentity struct {
	deviceMajor, deviceMinor uint32
	inode, mount             uint64
}

// Recovery reclaims a fixed host layout after independent service fencing.
// The host must reserve the root for this installation and coordinate writers.
// It never unmounts or recursively deletes content. No descriptors are retained
// between operations.
type Recovery struct {
	root     string
	owner    uint32
	identity recoveryIdentity
	fence    func(context.Context, id.ID) error
	kind     layout
}

func productionRecovery(root string, fence func(context.Context, id.ID) error, kind layout) (*Recovery, error) {
	if os.Geteuid() != 0 {
		return nil, fmt.Errorf("cortex: workspace recovery requires root on Linux")
	}
	return newRecovery(root, fence, 0, kind)
}

func newRecovery(root string, fence func(context.Context, id.ID) error, owner uint32, kind layout) (*Recovery, error) {
	if fence == nil || (kind != workspaceLayout && kind != credentialLayout) {
		return nil, fmt.Errorf("cortex: workspace recovery requires service fencing")
	}
	directory, err := hostfs.OpenDirectory(root, owner)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	identity, err := retainedIdentity(directory, owner)
	if err != nil {
		return nil, err
	}
	return &Recovery{root: root, owner: owner, identity: identity, fence: fence, kind: kind}, nil
}

func retainedIdentity(file *os.File, owner uint32) (recoveryIdentity, error) {
	stat, err := hostfs.MountIdentity(int(file.Fd()))
	if err != nil {
		return recoveryIdentity{}, err
	}
	if stat.Uid != owner || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o7777 != 0o700 {
		return recoveryIdentity{}, fmt.Errorf("cortex: retained workspace directory owner or mode changed")
	}
	return recoveryIdentity{deviceMajor: stat.Dev_major, deviceMinor: stat.Dev_minor, inode: stat.Ino, mount: stat.Mnt_id}, nil
}

func (r *Recovery) openRoot() (*os.File, error) {
	root, err := hostfs.OpenDirectory(r.root, r.owner)
	if err != nil {
		return nil, err
	}
	current, err := retainedIdentity(root, r.owner)
	if err != nil || current != r.identity {
		root.Close()
		return nil, errors.Join(err, fmt.Errorf("cortex: workspace recovery root changed"))
	}
	return root, nil
}

func (r *Recovery) openChild(parent *os.File, name string) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	child := os.NewFile(uintptr(fd), name)
	identity, err := retainedIdentity(child, r.owner)
	if err != nil || identity.mount != r.identity.mount || identity.deviceMajor != r.identity.deviceMajor || identity.deviceMinor != r.identity.deviceMinor {
		child.Close()
		return nil, errors.Join(err, fmt.Errorf("cortex: retained workspace is mounted or unsafe"))
	}
	return child, nil
}

func boundedNames(directory *os.File, maximum int) ([]string, error) {
	names, err := directory.Readdirnames(maximum + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(names) > maximum {
		return nil, fmt.Errorf("cortex: retained workspace entry limit exceeded")
	}
	slices.Sort(names)
	return names, nil
}

func (r *Recovery) discover(ctx context.Context, maximum int) ([]id.ID, error) {
	root, err := r.openRoot()
	if err != nil {
		return nil, err
	}
	defer root.Close()
	names, err := boundedNames(root, maximum)
	if err != nil {
		return nil, err
	}
	keys := make([]id.ID, 0, len(names))
	for _, name := range names {
		if checkErr := ctx.Err(); checkErr != nil {
			return nil, checkErr
		}
		key, parseErr := id.ParseWithPrefix(name, sandbox.PrefixJob)
		if parseErr != nil || key.IsNil() || key.String() != name {
			return nil, fmt.Errorf("cortex: ambiguous retained workspace identity")
		}
		child, openErr := r.openChild(root, name)
		if openErr != nil {
			return nil, openErr
		}
		if closeErr := child.Close(); closeErr != nil {
			return nil, closeErr
		}
		keys = append(keys, key)
	}
	return keys, ctx.Err()
}

func (r *Recovery) reclaim(ctx context.Context, key id.ID) error {
	root, err := r.openRoot()
	if err != nil {
		return err
	}
	defer root.Close()
	directory, err := r.openChild(root, key.String())
	if errors.Is(err, unix.ENOENT) {
		return errors.Join(root.Sync(), ctx.Err())
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	if r.kind == credentialLayout {
		return r.reclaimCredential(ctx, root, directory, key)
	}
	names, err := boundedNames(directory, 2)
	if err != nil {
		return err
	}
	children := make([]retainedEntry, 0, len(names))
	defer func() {
		for _, child := range children {
			child.file.Close()
		}
	}()
	// Check every child before removing any, so unexpected files remain intact.
	for _, name := range names {
		if name != "stage" && name != "scratch" {
			return fmt.Errorf("cortex: unexpected retained workspace entry")
		}
		child, openErr := r.openChild(directory, name)
		if openErr != nil {
			return openErr
		}
		children = append(children, retainedEntry{name: name, file: child})
		entries, readErr := boundedNames(child, 0)
		if readErr != nil || len(entries) != 0 {
			return errors.Join(readErr, fmt.Errorf("cortex: retained workspace mountpoint is not empty"))
		}
	}
	for _, child := range children {
		if checkErr := ctx.Err(); checkErr != nil {
			return checkErr
		}
		if checkErr := r.checkChild(directory, child); checkErr != nil {
			return checkErr
		}
		if checkErr := unix.Unlinkat(int(directory.Fd()), child.name, unix.AT_REMOVEDIR); checkErr != nil {
			return checkErr
		}
	}
	return r.removeDirectory(ctx, root, directory, key)
}

func (r *Recovery) removeDirectory(ctx context.Context, root, directory *os.File, key id.ID) error {
	if checkErr := ctx.Err(); checkErr != nil {
		return checkErr
	}
	currentRoot, err := r.openRoot()
	if err != nil {
		return err
	}
	if checkErr := currentRoot.Close(); checkErr != nil {
		return checkErr
	}
	if checkErr := r.checkChild(root, retainedEntry{name: key.String(), file: directory}); checkErr != nil {
		return checkErr
	}
	if checkErr := directory.Sync(); checkErr != nil {
		return checkErr
	}
	if checkErr := unix.Unlinkat(int(root.Fd()), key.String(), unix.AT_REMOVEDIR); checkErr != nil {
		return checkErr
	}
	return errors.Join(root.Sync(), ctx.Err())
}

func (r *Recovery) checkChild(parent *os.File, child retainedEntry) error {
	current, err := r.openChild(parent, child.name)
	if err != nil {
		return err
	}
	defer current.Close()
	expected, err := retainedIdentity(child.file, r.owner)
	if err != nil {
		return err
	}
	observed, err := retainedIdentity(current, r.owner)
	if err != nil || expected != observed {
		return errors.Join(err, fmt.Errorf("cortex: retained directory identity changed"))
	}
	return nil
}
