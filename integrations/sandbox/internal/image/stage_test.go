//go:build linux || darwin

package image

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	digest "github.com/opencontainers/go-digest"
	oci "github.com/opencontainers/image-spec/specs-go/v1"
)

type sourceFunc func(context.Context, string) (io.ReadCloser, error)

func (f sourceFunc) Open(ctx context.Context, pin string) (io.ReadCloser, error) { return f(ctx, pin) }

type entry struct {
	header tar.Header
	data   string
}

func layer(t *testing.T, entries ...entry) []byte {
	t.Helper()
	var out bytes.Buffer
	w := tar.NewWriter(&out)
	for _, e := range entries {
		e.header.Size = int64(len(e.data))
		if err := w.WriteHeader(&e.header); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.data)); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func pin(data []byte) string {
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:])
}

type imageFixture struct {
	cfg      Config
	blobs    map[string][]byte
	manifest oci.Manifest
	config   oci.Image
	image    string
}

func (f *imageFixture) seal(t *testing.T) {
	t.Helper()
	config, err := json.Marshal(f.config)
	if err != nil {
		t.Fatal(err)
	}
	f.manifest.Config = oci.Descriptor{MediaType: oci.MediaTypeImageConfig, Digest: digest.Digest(pin(config)), Size: int64(len(config))}
	f.blobs[pin(config)] = config
	body, err := json.Marshal(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	f.image = pin(body)
	f.blobs[f.image] = body
}

func fixture(t *testing.T, layers ...[]byte) *imageFixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	f := &imageFixture{blobs: map[string][]byte{}}
	f.config.OS, f.config.Architecture = "linux", runtime.GOARCH
	f.config.RootFS.Type = "layers"
	f.manifest.SchemaVersion, f.manifest.MediaType = 2, oci.MediaTypeImageManifest
	for _, raw := range layers {
		var out bytes.Buffer
		w := gzip.NewWriter(&out)
		if _, err = w.Write(raw); err != nil {
			t.Fatal(err)
		}
		if err = w.Close(); err != nil {
			t.Fatal(err)
		}
		body := out.Bytes()
		f.blobs[pin(body)] = body
		f.manifest.Layers = append(f.manifest.Layers, oci.Descriptor{MediaType: oci.MediaTypeImageLayerGzip, Digest: digest.Digest(pin(body)), Size: int64(len(body))})
		f.config.RootFS.DiffIDs = append(f.config.RootFS.DiffIDs, digest.Digest(pin(raw)))
	}
	f.seal(t)
	f.cfg = Config{Root: root, OwnerUID: uint32(os.Geteuid()), Architecture: runtime.GOARCH, CompressedBytes: 1 << 20, ExpandedBytes: 1 << 20, Entries: 100, Timeout: time.Second * 5,
		Authorize: func(context.Context, string) error { return nil },
		Source: sourceFunc(func(_ context.Context, pin string) (io.ReadCloser, error) {
			body, ok := f.blobs[pin]
			if !ok {
				return nil, os.ErrNotExist
			}
			return io.NopCloser(bytes.NewReader(body)), nil
		}),
	}
	return f
}

func regular(name, value string) entry {
	return entry{tar.Header{Name: name, Mode: 0o644, Typeflag: tar.TypeReg}, value}
}

func TestStageBindsManifestConfigAndLayers(t *testing.T) {
	f := fixture(t, layer(t, regular("etc/base", "first"), regular("etc/removed", "gone")), layer(t, regular("etc/base", "second"), regular("etc/.wh.removed", ""), entry{header: tar.Header{Name: "bin", Typeflag: tar.TypeSymlink, Linkname: "usr/bin", Mode: 0o777}}, regular("usr/bin/check", "binary")))
	// Image startup defaults must never replace the sealed job's argv or env.
	f.config.Config.Cmd = []string{"untrusted-default"}
	f.config.Config.Env = []string{"SECRET=untrusted"}
	f.seal(t)
	result, err := Stage(t.Context(), f.cfg, f.image)
	if err != nil {
		t.Fatal(err)
	}
	if result.Digest != f.image || filepath.Dir(result.Rootfs) != result.Directory {
		t.Fatalf("unbound image: %+v", result)
	}
	for name, want := range map[string]string{"etc/base": "second", "bin/check": "binary"} {
		got, readErr := os.ReadFile(filepath.Join(result.Rootfs, name))
		if readErr != nil || string(got) != want {
			t.Fatalf("%s = %q: %v", name, got, readErr)
		}
	}
	if _, err = os.Lstat(filepath.Join(result.Rootfs, "etc/removed")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("whiteout: %v", err)
	}
	entries, err := os.ReadDir(result.Directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "rootfs" {
		t.Fatalf("staging debris: %v %v", entries, err)
	}
}

func TestStageRejectsBrokenImageChain(t *testing.T) {
	for _, test := range []string{"manifest digest", "config digest", "layer digest", "layer size", "diff ID", "platform", "OS", "missing diff ID", "foreign URL", "embedded data", "unsupported compression", "index"} {
		t.Run(test, func(t *testing.T) {
			f := fixture(t, layer(t, regular("file", "original")))
			switch test {
			case "manifest digest":
				f.blobs[f.image] = []byte("substituted")
			case "config digest":
				f.blobs[f.manifest.Config.Digest.String()] = []byte("substituted")
			case "layer digest":
				f.blobs[f.manifest.Layers[0].Digest.String()] = []byte("substituted")
			case "layer size":
				f.manifest.Layers[0].Size++
				f.seal(t)
			case "diff ID":
				f.config.RootFS.DiffIDs[0] = digest.Digest("sha256:" + strings.Repeat("a", 64))
				f.seal(t)
			case "platform":
				f.config.Architecture = "invalid"
				f.seal(t)
			case "OS":
				f.config.OS = "windows"
				f.seal(t)
			case "missing diff ID":
				f.config.RootFS.DiffIDs = nil
				f.seal(t)
			case "foreign URL":
				f.manifest.Layers[0].URLs = []string{"https://invalid.example/image"}
				f.seal(t)
			case "embedded data":
				f.manifest.Layers[0].Data = []byte("untrusted")
				f.seal(t)
			case "unsupported compression":
				f.manifest.Layers[0].MediaType = "application/octet-stream"
				f.seal(t)
			case "index":
				f.manifest.MediaType = oci.MediaTypeImageIndex
				f.seal(t)
			}
			if result, err := Stage(t.Context(), f.cfg, f.image); err == nil || result != nil {
				t.Fatalf("invalid chain accepted: %+v %v", result, err)
			}
			assertEmpty(t, f.cfg.Root)
		})
	}
}

func TestStageRejectsUnsafeArchivesBeforeExtraction(t *testing.T) {
	for _, test := range []struct {
		name    string
		entries []entry
	}{
		{"traversal", []entry{regular("../escape", "bad")}},
		{"absolute path", []entry{regular("/escape", "bad")}},
		{"duplicate", []entry{regular("a", "one"), regular("./a", "two")}},
		{"setuid", []entry{{header: tar.Header{Name: "a", Typeflag: tar.TypeReg, Mode: 0o4755}}}},
		{"FIFO", []entry{{header: tar.Header{Name: "a", Typeflag: tar.TypeFifo, Mode: 0o644}}}},
		{"device", []entry{{header: tar.Header{Name: "a", Typeflag: tar.TypeChar, Mode: 0o644}}}},
		{"hardlink traversal", []entry{{header: tar.Header{Name: "a", Typeflag: tar.TypeLink, Linkname: "../escape", Mode: 0o644}}}},
		{"xattr", []entry{{header: tar.Header{Name: "a", Typeflag: tar.TypeReg, Mode: 0o644, Xattrs: map[string]string{"security.capability": "bad"}}}}},
		{"unsafe whiteout", []entry{regular(".wh...", "")}},
		{"nonempty whiteout", []entry{regular(".wh.a", "bad")}},
		{"too deep", []entry{regular(strings.Repeat("dir/", 65)+"a", "bad")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := fixture(t, layer(t, test.entries...))
			if result, err := Stage(t.Context(), f.cfg, f.image); err == nil || result != nil {
				t.Fatalf("unsafe archive accepted: %+v %v", result, err)
			}
			assertEmpty(t, f.cfg.Root)
		})
	}
}

func assertEmpty(t *testing.T, root string) {
	t.Helper()
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed staging left files: %v %v", entries, err)
	}
}

func TestStageBudgetsAndAuthorization(t *testing.T) {
	for _, test := range []string{"compressed", "expanded", "entries", "revoked", "cancelled"} {
		t.Run(test, func(t *testing.T) {
			f := fixture(t, layer(t, regular("first", strings.Repeat("a", 8192)), regular("second", "b")))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch test {
			case "compressed":
				f.cfg.CompressedBytes = 1
			case "expanded":
				f.cfg.ExpandedBytes = 1024
			case "entries":
				f.cfg.Entries = 1
			case "revoked":
				f.cfg.Authorize = func(context.Context, string) error { return os.ErrPermission }
			case "cancelled":
				cancel()
			}
			if result, err := Stage(ctx, f.cfg, f.image); err == nil || result != nil {
				t.Fatalf("admission accepted: %+v %v", result, err)
			}
			assertEmpty(t, f.cfg.Root)
		})
	}
}

func TestStageChargesImplicitDirectoriesAndAggregateLayers(t *testing.T) {
	for _, test := range []string{"parents", "layers"} {
		t.Run(test, func(t *testing.T) {
			raw := layer(t, regular("a/b/c/d/file", "value"))
			f := fixture(t, raw, raw)
			if test == "parents" {
				f.cfg.Entries = 4
			} else {
				f.cfg.ExpandedBytes = int64(len(raw))*2 - 1
			}
			if result, err := Stage(t.Context(), f.cfg, f.image); err == nil || result != nil {
				t.Fatalf("budget bypass: %+v %v", result, err)
			}
			assertEmpty(t, f.cfg.Root)
		})
	}
}

func TestLayerLinksAndOpaqueWhiteoutsRemainInsideRootfs(t *testing.T) {
	outside := t.TempDir()
	canary := filepath.Join(outside, "canary")
	if err := os.WriteFile(canary, []byte("host"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := fixture(t,
		layer(t, regular("dir/old", "old"), regular("shared", "original")),
		layer(t, regular("dir/new", "new"), regular("dir/.wh..wh..opq", ""),
			entry{header: tar.Header{Name: "copy", Typeflag: tar.TypeLink, Linkname: "shared", Mode: 0o644}},
			entry{header: tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777}},
			regular("escape/canary", "guest")))
	result, err := Stage(t.Context(), f.cfg, f.image)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(canary)
	if err != nil || string(got) != "host" {
		t.Fatalf("host canary changed: %q %v", got, err)
	}
	for name, want := range map[string]string{"dir/new": "new", "copy": "original", strings.TrimPrefix(canary, "/"): "guest"} {
		got, err = os.ReadFile(filepath.Join(result.Rootfs, name))
		if err != nil || string(got) != want {
			t.Fatalf("guest %s: %q %v", name, got, err)
		}
	}
	if _, err = os.Lstat(filepath.Join(result.Rootfs, "dir/old")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("opaque whiteout left old entry: %v", err)
	}
}

func TestStageUncompressedLayerAndTrailingContent(t *testing.T) {
	for _, trailing := range []bool{false, true} {
		t.Run(fmt.Sprintf("trailing-%v", trailing), func(t *testing.T) {
			raw := layer(t, regular("file", "value"))
			if trailing {
				raw = append(raw, []byte("hidden payload")...)
			}
			f := fixture(t, raw)
			f.blobs[pin(raw)] = raw
			f.manifest.Layers[0] = oci.Descriptor{MediaType: oci.MediaTypeImageLayer, Digest: digest.Digest(pin(raw)), Size: int64(len(raw))}
			f.seal(t)
			result, err := Stage(t.Context(), f.cfg, f.image)
			if trailing {
				if err == nil || result != nil {
					t.Fatalf("trailing content accepted: %+v %v", result, err)
				}
				assertEmpty(t, f.cfg.Root)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

type failingClose struct {
	io.Reader
	err error
}

func (r failingClose) Close() error { return r.err }

func TestImageSourceCancellationAndCloseFailure(t *testing.T) {
	for _, test := range []string{"cancel", "close", "revoked after read"} {
		t.Run(test, func(t *testing.T) {
			f := fixture(t, layer(t, regular("file", "value")))
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			original := f.cfg.Source
			closeFailure := errors.New("close failure")
			f.cfg.Source = sourceFunc(func(ctx context.Context, pin string) (io.ReadCloser, error) {
				if pin != f.manifest.Layers[0].Digest.String() {
					return original.Open(ctx, pin)
				}
				switch test {
				case "cancel":
					r, w := io.Pipe()
					t.Cleanup(func() { _ = w.Close() })
					cancel()
					return r, nil
				case "close":
					return failingClose{bytes.NewReader(f.blobs[pin]), closeFailure}, nil
				default:
					return original.Open(ctx, pin)
				}
			})
			if test == "revoked after read" {
				calls := 0
				f.cfg.Authorize = func(context.Context, string) error {
					calls++
					if calls >= 5 {
						return os.ErrPermission
					}
					return nil
				}
			}
			result, err := Stage(ctx, f.cfg, f.image)
			if err == nil || result != nil {
				t.Fatalf("source failure accepted: %+v %v", result, err)
			}
			if test == "cancel" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if test == "close" && !errors.Is(err, closeFailure) {
				t.Fatalf("lost close error: %v", err)
			}
			assertEmpty(t, f.cfg.Root)
		})
	}
}

func TestRejectAmbiguousOCIMetadata(t *testing.T) {
	for _, field := range []string{"mediaType", "MEDIATYPE", "ſchemaVersion"} {
		t.Run(field, func(t *testing.T) {
			f := fixture(t, layer(t, regular("file", "value")))
			body := append([]byte(`{"`+field+`":"ignored",`), f.blobs[f.image][1:]...)
			f.image = pin(body)
			f.blobs[f.image] = body
			if result, err := Stage(t.Context(), f.cfg, f.image); err == nil || result != nil {
				t.Fatalf("ambiguous metadata accepted: %+v %v", result, err)
			}
			assertEmpty(t, f.cfg.Root)
		})
	}
}
