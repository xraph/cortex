//go:build linux || darwin

package deployment

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"runtime"

	"golang.org/x/sys/unix"

	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

// Verify checks a dedicated installation against a trusted manifest without
// executing it. All files and installation directories must belong to ownerUID;
// the production root-run worker must supply zero. Ancestors must belong to root
// or that owner and deny other writers, except root-owned sticky directories.
// Resolve any administrative path aliases before configuring the directory.
//
// The trusted owner must keep the installation unchanged during verification
// and use. This is not a lock against that owner's later writes. Recheck before
// dispatch, and never allow workloads to write any part of the installation.
func Verify(ctx context.Context, directory string, manifest Manifest, ownerUID uint32) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	digest, err := manifest.Digest()
	if err != nil {
		return "", err
	}
	if manifest.Architecture != runtime.GOARCH {
		return "", fmt.Errorf("cortex: runtime deployment architecture mismatch")
	}
	root, err := hostfs.OpenDirectory(directory, ownerUID)
	if err != nil {
		return "", err
	}
	defer root.Close()
	files := make(map[string]File, len(manifest.Files))
	directories := map[string]bool{".": true}
	for _, file := range manifest.Files {
		files[file.Path] = file
		for parent := path.Dir(file.Path); parent != "."; parent = path.Dir(parent) {
			directories[parent] = true
		}
	}
	if verifyErr := verifyDirectory(ctx, root, ".", ownerUID, files, directories); verifyErr != nil {
		return "", verifyErr
	}
	if len(files) != 0 || len(directories) != 0 {
		return "", fmt.Errorf("cortex: runtime deployment inventory is incomplete")
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return "", contextErr
	}
	return digest, nil
}

func verifyDirectory(ctx context.Context, directory *os.File, name string, owner uint32, files map[string]File, directories map[string]bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(directory.Fd()), &stat); err != nil {
		return err
	}
	if !directories[name] || stat.Uid != owner || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Mode&0o022 != 0 || stat.Mode&0o555 != 0o555 || stat.Mode&(unix.S_ISUID|unix.S_ISGID|unix.S_ISVTX) != 0 {
		return fmt.Errorf("cortex: unsafe or unexpected runtime deployment directory")
	}
	delete(directories, name)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		entries, err := directory.ReadDir(1)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		child := path.Join(name, entries[0].Name())
		file, isFile := files[child]
		if !isFile && !directories[child] {
			return fmt.Errorf("cortex: unexpected runtime deployment entry")
		}
		flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
		if !isFile {
			flags |= unix.O_DIRECTORY
		}
		fd, err := unix.Openat(int(directory.Fd()), entries[0].Name(), flags, 0)
		if err != nil {
			return err
		}
		opened := os.NewFile(uintptr(fd), child)
		if isFile {
			err = verifyFile(ctx, opened, file, owner)
			delete(files, child)
		} else {
			err = verifyDirectory(ctx, opened, child, owner, files, directories)
		}
		if operationErr := errors.Join(err, opened.Close()); operationErr != nil {
			return operationErr
		}
	}
}

func verifyFile(ctx context.Context, file *os.File, expected File, owner uint32) error {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Uid != owner || uint64(stat.Mode)&0o7777 != uint64(expected.Mode) || stat.Size != expected.Bytes {
		return fmt.Errorf("cortex: unsafe or mismatched runtime deployment file")
	}
	hash := sha256.New()
	reader := io.LimitReader(file, expected.Bytes+1)
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := reader.Read(buffer)
		if n > 0 {
			total += int64(n)
			if _, writeErr := hash.Write(buffer[:n]); writeErr != nil {
				return writeErr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
	}
	if total != expected.Bytes || hex.EncodeToString(hash.Sum(nil)) != expected.SHA256 {
		return fmt.Errorf("cortex: runtime deployment content mismatch")
	}
	return ctx.Err()
}
