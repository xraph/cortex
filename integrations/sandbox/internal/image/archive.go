//go:build linux || darwin

package image

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"unicode/utf8"
)

type archiveBudget struct {
	bytes   int64
	entries int
}

func inspect(ctx context.Context, reader io.Reader, budget *archiveBudget, maximum int64) error {
	reader = contextReader{ctx, reader}
	archive := tar.NewReader(reader)
	names := make(map[string]struct{})
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		// Charge potential implicit directories as well as explicit entries.
		// Repeated parents are deliberately charged again, bounding work across
		// layers and paths resolved through guest symlinks.
		budget.entries -= 1 + strings.Count(header.Name, "/") + strings.Count(header.Linkname, "/")
		if budget.entries < 0 {
			return fmt.Errorf("cortex: image exceeds entry budget")
		}
		name, err := archiveName(header.Name)
		if err != nil {
			return err
		}
		if _, duplicate := names[name]; duplicate {
			return fmt.Errorf("cortex: duplicate image entry %q", name)
		}
		names[name] = struct{}{}
		if operationErr := inspectHeader(header, name); operationErr != nil {
			return operationErr
		}
		// Consume content so truncated entries cannot pass the preflight.
		if header.Size < 0 || header.Size > maximum {
			return fmt.Errorf("cortex: image entry exceeds expanded layer")
		}
		if _, err = io.CopyN(io.Discard, archive, header.Size); err != nil {
			return err
		}
	}
	// A second archive or hidden nonzero bytes after the end marker would not be
	// applied. Refuse them so every verified payload has one interpretation.
	buffer := make([]byte, 32<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := reader.Read(buffer)
		for _, value := range buffer[:n] {
			if value != 0 {
				return fmt.Errorf("cortex: trailing image archive content")
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func archiveName(name string) (string, error) {
	if name == "" || len(name) > 4096 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\x00") || strings.HasPrefix(name, "/") || strings.Count(name, "/") > 64 {
		return "", fmt.Errorf("cortex: invalid image entry path")
	}
	for _, part := range strings.Split(name, "/") {
		if part == ".." {
			return "", fmt.Errorf("cortex: image path escapes root")
		}
	}
	return path.Clean(name), nil
}

func inspectHeader(header *tar.Header, name string) error {
	if header.Mode < 0 || header.Mode&0o6000 != 0 || header.Mode&^int64(0o7777) != 0 {
		return fmt.Errorf("cortex: unsafe image permissions or attributes")
	}
	for key := range header.PAXRecords {
		switch key {
		case "path", "linkpath", "size", "uid", "gid", "uname", "gname", "mtime", "atime", "ctime":
		default:
			return fmt.Errorf("cortex: unsupported image extended attribute %q", key)
		}
	}
	if name == "." && header.Typeflag != tar.TypeDir {
		return fmt.Errorf("cortex: non-directory image root")
	}
	if strings.HasPrefix(path.Base(name), ".wh.") {
		target := strings.TrimPrefix(path.Base(name), ".wh.")
		if header.Typeflag != tar.TypeReg || header.Size != 0 || target == "" || target == "." || target == ".." || (strings.HasPrefix(target, ".wh.") && path.Base(name) != ".wh..wh..opq") {
			return fmt.Errorf("cortex: invalid image whiteout")
		}
	}
	switch header.Typeflag {
	case tar.TypeReg, tar.TypeDir:
		if header.Linkname != "" {
			return fmt.Errorf("cortex: unexpected image link target")
		}
	case tar.TypeSymlink:
		// Absolute and parent-relative targets have guest-root semantics. The
		// maintained extractor resolves parent paths within rootfs during apply.
		if header.Linkname == "" || len(header.Linkname) > 4096 || strings.Count(header.Linkname, "/") > 64 || !utf8.ValidString(header.Linkname) || strings.ContainsAny(header.Linkname, "\\\x00") {
			return fmt.Errorf("cortex: invalid image symlink")
		}
	case tar.TypeLink:
		if _, err := archiveName(header.Linkname); err != nil {
			return err
		}
	default:
		return fmt.Errorf("cortex: unsupported image entry type %d", header.Typeflag)
	}
	return nil
}
