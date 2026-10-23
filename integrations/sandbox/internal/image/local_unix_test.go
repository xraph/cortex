//go:build linux || darwin

package image

import (
	"archive/tar"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func localFixture(t *testing.T, data []byte, maximum int64) (*LocalSource, string, string) {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "blobs", "sha256")
	if err = os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(directory, pin(data)[7:])
	if err = os.WriteFile(name, data, 0o400); err != nil {
		t.Fatal(err)
	}
	source, err := newLocalSource(root, maximum, 1, uint32(os.Geteuid()))
	if err != nil {
		t.Fatal(err)
	}
	return source, root, name
}

func TestLocalSourceSnapshotAndCapacity(t *testing.T) {
	data := []byte("approved image blob")
	source, _, name := localFixture(t, data, 1024)
	reader, err := source.Open(t.Context(), pin(data))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	// A verified snapshot remains stable when the host replaces the blob.
	if err = os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(name, []byte("changed"), 0o400); err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if err != nil || string(body) != string(data) {
		t.Fatal(string(body), err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	if other, openErr := source.Open(ctx, pin(data)); !errors.Is(openErr, context.DeadlineExceeded) || other != nil {
		t.Fatal("capacity wait ignored cancellation", openErr)
	}
	if err = reader.Close(); err != nil {
		t.Fatal(err)
	}
	if err = reader.Close(); err != nil {
		t.Fatal("repeated close", err)
	}
	if _, err = reader.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatal("closed snapshot readable", err)
	}
	if other, openErr := source.Open(t.Context(), pin(data)); openErr == nil || other != nil {
		t.Fatal("changed bytes accepted")
	}
	// Failure must return the slot as well.
	if err = os.Remove(name); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(name, data, 0o400); err != nil {
		t.Fatal(err)
	}
	again, err := source.Open(t.Context(), pin(data))
	if err != nil {
		t.Fatal(err)
	}
	again.Close()
}

func TestLocalSourceRejectsUnsafeContent(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "fifo", "directory", "writable", "large", "parent alias", "root replacement", "public directory", "foreign owner"} {
		t.Run(kind, func(t *testing.T) {
			data := []byte("approved")
			source, root, name := localFixture(t, data, int64(len(data)))
			var err error
			switch kind {
			case "symlink", "fifo", "directory":
				if err = os.Remove(name); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "symlink":
					err = os.Symlink("/etc/passwd", name)
				case "fifo":
					err = unix.Mkfifo(name, 0o400)
				case "directory":
					err = os.Mkdir(name, 0o700)
				}
			case "hardlink":
				err = os.Link(name, name+"-alias")
			case "writable":
				err = os.Chmod(name, 0o600)
			case "large":
				source.maximum--
			case "parent alias":
				original := filepath.Join(root, "blobs")
				if err = os.Rename(original, original+"-old"); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink(original+"-old", original)
			case "root replacement":
				if err = os.Rename(root, root+"-old"); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { os.RemoveAll(root + "-old") })
				err = os.MkdirAll(filepath.Dir(name), 0o700)
				if err == nil {
					err = os.WriteFile(name, data, 0o400)
				}
			case "public directory":
				err = os.Chmod(filepath.Dir(name), 0o755)
			case "foreign owner":
				source.owner++
			}
			if err != nil {
				t.Fatal(err)
			}
			if reader, openErr := source.Open(t.Context(), pin(data)); openErr == nil || reader != nil {
				t.Fatal("unsafe source accepted", openErr)
			}
		})
	}
}

func TestLocalSourceRejectsInvalidConfigurationAndDigests(t *testing.T) {
	source, root, _ := localFixture(t, []byte("blob"), 10)
	for _, empty := range []*LocalSource{nil, {}} {
		if reader, err := empty.Open(t.Context(), pin([]byte("blob"))); err == nil || reader != nil {
			t.Fatal("empty source accepted")
		}
	}
	for _, value := range []string{"", "../escape", "sha256:" + strings.Repeat("A", 64), "https://registry/blob", strings.Repeat("a", 64)} {
		if reader, err := source.Open(t.Context(), value); err == nil || reader != nil {
			t.Fatal("invalid digest accepted")
		}
	}
	for _, limits := range [][2]int64{{0, 1}, {65 << 20, 1}, {1, 0}, {1, 33}, {64 << 20, 5}} {
		if other, err := newLocalSource(root, limits[0], int(limits[1]), uint32(os.Geteuid())); err == nil || other != nil {
			t.Fatal("invalid limit accepted", limits)
		}
	}
	if other, err := newLocalSource(root+"/", 10, 1, uint32(os.Geteuid())); err == nil || other != nil {
		t.Fatal("noncanonical root accepted")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if reader, err := source.Open(ctx, pin([]byte("blob"))); !errors.Is(err, context.Canceled) || reader != nil {
		t.Fatal("cancelled open", err)
	}
	reader, err := source.Open(t.Context(), pin([]byte("blob")))
	if err != nil {
		t.Fatal(err)
	}
	reader.Close()
}

func TestLocalSourceStagesVerifiedImage(t *testing.T) {
	f := fixture(t, layer(t, entry{header: tar.Header{Name: "hello.txt", Mode: 0o644, Typeflag: tar.TypeReg}, data: "hello"}))
	source, root, _ := localFixture(t, []byte("unreferenced blob"), 1<<20)
	for digest, data := range f.blobs {
		if err := os.WriteFile(filepath.Join(root, "blobs", "sha256", digest[7:]), data, 0o400); err != nil {
			t.Fatal(err)
		}
	}
	f.cfg.Source = source
	result, err := Stage(t.Context(), f.cfg, f.image)
	if err != nil || result == nil {
		t.Fatal("local source did not stage image", err)
	}
	body, err := os.ReadFile(filepath.Join(result.Rootfs, "hello.txt"))
	if err != nil || string(body) != "hello" {
		t.Fatal("wrong staged content", err)
	}
}

func TestLocalSourceConcurrentReadsAndClose(t *testing.T) {
	data := []byte(strings.Repeat("payload", 100))
	source, _, _ := localFixture(t, data, 1024)
	ctx, cancel := context.WithCancel(t.Context())
	reader, err := source.Open(ctx, pin(data))
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	cancel()
	if _, err = reader.Read(make([]byte, 1)); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled snapshot readable", err)
	}
	reader.Close()
	var wg sync.WaitGroup
	for range 16 {
		wg.Go(func() {
			operation, stop := context.WithTimeout(t.Context(), time.Second)
			defer stop()
			current, openErr := source.Open(operation, pin(data))
			if openErr != nil {
				t.Error(openErr)
				return
			}
			body, readErr := io.ReadAll(current)
			if readErr != nil || string(body) != string(data) {
				t.Error("concurrent snapshot mismatch", readErr)
			}
			var closers sync.WaitGroup
			for range 4 {
				closers.Go(func() { current.Close() })
			}
			closers.Wait()
		})
	}
	wg.Wait()
}
