//go:build linux || darwin

package image

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/xraph/cortex/integrations/sandbox/internal/hostfs"
)

// LocalSource reads installed blobs from root/blobs/sha256/<digest>. It uses
// only digest references, not OCI index tags or registry URLs. The protected
// host owns provisioning; source access grants remain the caller's responsibility.
// Each successful Open holds a capacity slot until its reader is closed.
type LocalSource struct {
	root        string
	owner       uint32
	maximum     int64
	slots       chan struct{}
	directories [3]os.FileInfo
}

// NewLocalSource requires root-owned mode-0700 content directories and mode-0400
// single-link blobs. maximum bounds each snapshot to 64 MiB; readers is 1..32,
// with at most 256 MiB of aggregate snapshot capacity. Keep this installation
// on a responsive local filesystem: Open checks cancellation between file calls
// but cannot interrupt a stalled kernel call. Returned reads use only memory.
// The trusted owner must prevent path and file mutations during Open.
func NewLocalSource(root string, maximum int64, readers int) (*LocalSource, error) {
	return newLocalSource(root, maximum, readers, 0)
}

// LocalLimits bounds verified image snapshots and aggregate reader capacity.
type LocalLimits struct {
	MaxBytes int64 `json:"max_bytes"`
	Readers  int   `json:"readers"`
}

// Validate checks per-blob, reader and aggregate snapshot bounds.
func (l LocalLimits) Validate() error {
	if l.MaxBytes <= 0 || l.MaxBytes > 64<<20 || l.Readers < 1 || l.Readers > 32 || l.MaxBytes*int64(l.Readers) > 256<<20 {
		return fmt.Errorf("cortex: invalid local image source limits")
	}
	return nil
}

func newLocalSource(root string, maximum int64, readers int, owner uint32) (*LocalSource, error) {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root || (LocalLimits{MaxBytes: maximum, Readers: readers}).Validate() != nil {
		return nil, fmt.Errorf("cortex: invalid local image source configuration")
	}
	source := &LocalSource{root: root, maximum: maximum, owner: owner, slots: make(chan struct{}, readers)}
	if err := source.checkDirectories(true); err != nil {
		return nil, err
	}
	return source, nil
}

func (s *LocalSource) checkDirectories(initialize bool) error {
	for i, name := range []string{s.root, filepath.Join(s.root, "blobs"), filepath.Join(s.root, "blobs", "sha256")} {
		directory, err := hostfs.OpenDirectory(name, s.owner)
		if err != nil {
			return err
		}
		info, statErr := directory.Stat()
		if operationErr := errors.Join(statErr, directory.Close()); operationErr != nil {
			return operationErr
		}
		if info.Mode().Perm() != 0o700 || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return fmt.Errorf("cortex: local image directory must be private")
		}
		if initialize {
			s.directories[i] = info
		} else if !os.SameFile(s.directories[i], info) {
			return fmt.Errorf("cortex: local image directory replaced")
		}
	}
	return nil
}

// Open verifies the digest and protected file metadata before returning a
// bounded snapshot. Errors return no reader or content, and release capacity.
func (s *LocalSource) Open(ctx context.Context, digest string) (io.ReadCloser, error) {
	if s == nil || s.slots == nil || !ValidDigest(digest) {
		return nil, fmt.Errorf("cortex: invalid local image request")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	retained := false
	defer func() {
		if !retained {
			<-s.slots
		}
	}()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.checkDirectories(false); err != nil {
		return nil, err
	}
	name := filepath.Join(s.root, "blobs", "sha256", digest[7:])
	// Lstat only obtains a size hint. ReadPin reopens through verified no-follow
	// parents and checks the actual descriptor's size, owner, type, links and hash.
	info, err := os.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > s.maximum {
		return nil, fmt.Errorf("cortex: invalid local image blob size or type")
	}
	body, err := hostfs.ReadPin(ctx, hostfs.Pin{Path: name, SHA256: digest[7:], Bytes: info.Size(), Mode: 0o400}, s.owner)
	if err != nil {
		return nil, err
	}
	if directoryErr := s.checkDirectories(false); directoryErr != nil {
		return nil, directoryErr
	}
	if contextErr := ctx.Err(); contextErr != nil {
		return nil, contextErr
	}
	retained = true
	return &localReader{ctx: ctx, reader: bytes.NewReader(body), release: func() { <-s.slots }}, nil
}

type localReader struct {
	mu      sync.Mutex
	ctx     context.Context
	reader  *bytes.Reader
	release func()
}

func (r *localReader) Read(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reader == nil {
		return 0, os.ErrClosed
	}
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func (r *localReader) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reader != nil {
		r.reader = nil
		r.release()
		r.release = nil
	}
	return nil
}
