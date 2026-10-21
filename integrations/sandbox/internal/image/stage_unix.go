//go:build linux || darwin

// Package image stages approved OCI image filesystems without running code.
package image

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/containerd/containerd/v2/pkg/archive"
	oci "github.com/opencontainers/image-spec/specs-go/v1"

	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
	"github.com/xraph/cortex/integrations/sandbox/internal/verifiedio"
)

// Source opens a digest from the host's approved content service. It must honor
// cancellation, and Close must unblock Read. It never receives image URLs.
type Source interface {
	Open(context.Context, string) (io.ReadCloser, error)
}

// Config is trusted host policy. Authorize must bind the authenticated context
// to current image access and an approved manifest digest. Root must be private;
// its owner must keep staged files unchanged until the workload is destroyed.
// Production workers use OwnerUID zero. Limits are aggregate across all layers;
// compressed bytes include the manifest and config. Timeout is at most ten minutes.
type Config struct {
	Root, Architecture             string
	OwnerUID                       uint32
	Source                         Source
	Authorize                      func(context.Context, string) error
	CompressedBytes, ExpandedBytes int64
	Entries                        int
	Timeout                        time.Duration
}

// Filesystem binds a private, newly staged rootfs to its approved manifest. It
// is not a durable cache entry, job claim or runtime qualification. The host
// owns Directory's lifecycle and must not follow guest symlinks from the host.
type Filesystem struct {
	Digest, Directory, Rootfs string
}

// Stage supports a single-platform OCI manifest with tar or gzip layers. It
// verifies compressed digests, uncompressed DiffIDs and archive policy before
// applying each layer with containerd's confined OCI extractor. Image process
// defaults are never returned or applied. Failed attempts remove only their
// own temporary directory; retries create a fresh copy and reauthorize access.
func Stage(ctx context.Context, cfg Config, image string) (result *Filesystem, err error) {
	if cfg.Source == nil || cfg.Authorize == nil || !validDigest(image) || int64(cfg.OwnerUID) != int64(os.Geteuid()) || cfg.Architecture != runtime.GOARCH || (cfg.Architecture != "amd64" && cfg.Architecture != "arm64") || cfg.CompressedBytes <= 0 || cfg.CompressedBytes > 4<<30 || cfg.ExpandedBytes <= 0 || cfg.ExpandedBytes > 8<<30 || cfg.Entries <= 0 || cfg.Entries > 100_000 || cfg.Timeout <= 0 || cfg.Timeout > 10*time.Minute {
		return nil, fmt.Errorf("cortex: invalid image staging configuration")
	}
	ctx, cancel := context.WithTimeout(ctx, cfg.Timeout)
	defer cancel()
	if operationErr := authorized(ctx, cfg, image); operationErr != nil {
		return nil, operationErr
	}
	root, err := hostfs.OpenDirectory(cfg.Root, cfg.OwnerUID)
	if err != nil {
		return nil, err
	}
	info, statErr := root.Stat()
	if closeErr := errors.Join(statErr, root.Close()); closeErr != nil {
		return nil, closeErr
	}
	if info.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("cortex: image staging root must be private")
	}
	compressed := cfg.CompressedBytes
	body, err := metadata(ctx, cfg, image, image, -1, &compressed)
	if err != nil {
		return nil, err
	}
	var manifest oci.Manifest
	if operationErr := decode(body, &manifest); operationErr != nil {
		return nil, operationErr
	}
	if manifest.SchemaVersion != 2 || manifest.MediaType != oci.MediaTypeImageManifest || manifest.ArtifactType != "" || manifest.Subject != nil || len(manifest.Layers) > 128 {
		return nil, fmt.Errorf("cortex: expected bounded single-platform OCI image manifest")
	}
	if operationErr := descriptor(manifest.Config, oci.MediaTypeImageConfig); operationErr != nil {
		return nil, operationErr
	}
	remaining := compressed
	if manifest.Config.Size > remaining || manifest.Config.Size > 1<<20 {
		return nil, fmt.Errorf("cortex: image config exceeds budget")
	}
	remaining -= manifest.Config.Size
	for _, desc := range manifest.Layers {
		if operationErr := descriptor(desc, oci.MediaTypeImageLayer, oci.MediaTypeImageLayerGzip); operationErr != nil {
			return nil, operationErr
		}
		if desc.Size > remaining {
			return nil, fmt.Errorf("cortex: image layers exceed compressed budget")
		}
		remaining -= desc.Size
	}
	body, err = metadata(ctx, cfg, image, manifest.Config.Digest.String(), manifest.Config.Size, &compressed)
	if err != nil {
		return nil, err
	}
	var config oci.Image
	if operationErr := decode(body, &config); operationErr != nil {
		return nil, operationErr
	}
	if config.OS != "linux" || config.Architecture != cfg.Architecture || config.Variant != "" || config.OSVersion != "" || len(config.OSFeatures) != 0 || config.RootFS.Type != "layers" || len(config.RootFS.DiffIDs) != len(manifest.Layers) {
		return nil, fmt.Errorf("cortex: unsupported image platform or rootfs")
	}
	for _, diffID := range config.RootFS.DiffIDs {
		if !validDigest(diffID.String()) {
			return nil, fmt.Errorf("cortex: unsupported layer DiffID")
		}
	}
	directory, err := os.MkdirTemp(cfg.Root, "image-")
	if err != nil {
		return nil, err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, remove(directory))
		}
	}()
	rootfs := filepath.Join(directory, "rootfs")
	if operationErr := os.Mkdir(rootfs, 0o755); operationErr != nil { //nolint:gosec // Guest traversal; enclosing staging directory is private 0700.
		return nil, operationErr
	}
	budget := archiveBudget{bytes: cfg.ExpandedBytes, entries: cfg.Entries}
	for i, desc := range manifest.Layers {
		if operationErr := applyLayer(ctx, cfg, image, desc, config.RootFS.DiffIDs[i].String(), directory, rootfs, &budget); operationErr != nil {
			return nil, operationErr
		}
	}
	if operationErr := authorized(ctx, cfg, image); operationErr != nil {
		return nil, operationErr
	}
	return &Filesystem{Digest: image, Directory: directory, Rootfs: rootfs}, nil
}

func authorized(ctx context.Context, cfg Config, image string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cfg.Authorize(ctx, image); err != nil {
		return err
	}
	return ctx.Err()
}

func validDigest(value string) bool {
	if len(value) != 71 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, ch := range value[7:] {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			return false
		}
	}
	return true
}

func descriptor(desc oci.Descriptor, mediaTypes ...string) error {
	if !validDigest(desc.Digest.String()) || desc.Size < 0 || len(desc.URLs) != 0 || len(desc.Data) != 0 || desc.Platform != nil || desc.ArtifactType != "" {
		return fmt.Errorf("cortex: unsupported image descriptor")
	}
	for _, mediaType := range mediaTypes {
		if desc.MediaType == mediaType {
			return nil
		}
	}
	return fmt.Errorf("cortex: unsupported image media type %q", desc.MediaType)
}

func decode(body []byte, target any) error {
	if err := unambiguousJSON(body); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("cortex: invalid OCI metadata: %w", err)
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return fmt.Errorf("cortex: trailing OCI metadata")
	}
	return nil
}

func metadata(ctx context.Context, cfg Config, image, pin string, size int64, remaining *int64) ([]byte, error) {
	var buffer bytes.Buffer
	if err := fetch(ctx, cfg, image, pin, size, min(*remaining, 1<<20), &buffer); err != nil {
		return nil, err
	}
	*remaining -= int64(buffer.Len())
	return buffer.Bytes(), nil
}

func fetch(ctx context.Context, cfg Config, image, pin string, size, maximum int64, destination io.Writer) error {
	if err := authorized(ctx, cfg, image); err != nil {
		return err
	}
	reader, err := cfg.Source.Open(ctx, pin)
	if err != nil {
		if reader != nil {
			err = errors.Join(err, reader.Close())
		}
		return err
	}
	if reader == nil {
		return fmt.Errorf("cortex: image source returned no reader")
	}
	return verifiedio.Read(ctx, reader, destination, size, maximum, pin[7:])
}

func applyLayer(ctx context.Context, cfg Config, image string, desc oci.Descriptor, diffID, directory, rootfs string, budget *archiveBudget) (err error) {
	compressed, err := os.CreateTemp(directory, "blob-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, compressed.Close(), os.Remove(compressed.Name())) }()
	if operationErr := fetch(ctx, cfg, image, desc.Digest.String(), desc.Size, desc.Size, compressed); operationErr != nil {
		return operationErr
	}
	if _, err = compressed.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var reader io.Reader = compressed
	if desc.MediaType == oci.MediaTypeImageLayerGzip {
		unzip, openErr := gzip.NewReader(compressed)
		if openErr != nil {
			return openErr
		}
		defer unzip.Close()
		reader = unzip
	}
	expanded, err := os.CreateTemp(directory, "layer-")
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, expanded.Close(), os.Remove(expanded.Name())) }()
	if operationErr := verifiedio.CopyLimit(ctx, reader, expanded, -1, budget.bytes, diffID[7:]); operationErr != nil {
		return operationErr
	}
	length, err := expanded.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	budget.bytes -= length
	if _, err = expanded.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if operationErr := inspect(ctx, expanded, budget, length); operationErr != nil {
		return operationErr
	}
	if _, err = expanded.Seek(0, io.SeekStart); err != nil {
		return err
	}
	if operationErr := authorized(ctx, cfg, image); operationErr != nil {
		return operationErr
	}
	_, err = archive.Apply(ctx, rootfs, expanded, archive.WithNoSameOwner())
	return err
}

// remove can traverse image directories whose original modes deny owner access.
// WalkDir never follows guest symlinks. The trusted owner excludes concurrent
// modification, including renames of Root and this private staging directory.
func remove(directory string) error {
	walkErr := filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return os.Chmod(path, 0o700) //nolint:gosec // This is a directory and needs owner traversal for cleanup.
		}
		return nil
	})
	return errors.Join(walkErr, os.RemoveAll(directory))
}
