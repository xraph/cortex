//go:build linux || darwin

package sandboxbroker

import (
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"golang.org/x/sys/unix"
)

// ReadExport reads a bounded regular file through no-follow descriptors. Every
// component is opened relative to its verified parent, so rename and symlink
// swaps cannot redirect traversal outside the host-selected export directory.
// Call this after the workload has stopped; a live writer cannot produce an
// immutable snapshot. Archives are not expanded and hard links are rejected.
func ReadExport(directory, name string, maximum int64) ([]byte, error) {
	if maximum <= 0 || maximum > 1<<20 || name == "." || strings.HasPrefix(name, "/") || path.Clean(name) != name {
		return nil, fmt.Errorf("cortex: invalid sandbox export")
	}
	parts := strings.Split(name, "/")
	for _, part := range parts {
		if part == "" || part == ".." {
			return nil, fmt.Errorf("cortex: invalid sandbox export path")
		}
	}
	root, err := unix.Open(directory, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	descriptors := []int{root}
	defer func() {
		for _, fd := range descriptors {
			_ = unix.Close(fd)
		}
	}()
	parent := root
	for _, part := range parts[:len(parts)-1] {
		var next int
		next, err = unix.Openat(parent, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if err != nil {
			return nil, err
		}
		descriptors = append(descriptors, next)
		parent = next
	}
	fd, err := unix.Openat(parent, parts[len(parts)-1], unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	defer file.Close()
	var stat unix.Stat_t
	if operationErr := unix.Fstat(fd, &stat); operationErr != nil {
		return nil, operationErr
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 || stat.Size > maximum {
		return nil, fmt.Errorf("cortex: unsafe sandbox export file")
	}
	data, err := io.ReadAll(io.LimitReader(file, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("cortex: sandbox export exceeds byte limit")
	}
	return data, nil
}
