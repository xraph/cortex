package verifiedio

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
)

// Snapshot takes ownership of verified bytes and returns a cancellation-aware
// memory reader. Close releases its bytes and calls release exactly once. Reads
// and Close may run concurrently. The caller must not mutate data after transfer.
func Snapshot(ctx context.Context, data []byte, release func()) io.ReadCloser {
	return &snapshot{ctx: ctx, reader: bytes.NewReader(data), release: release}
}

type snapshot struct {
	mu      sync.Mutex
	ctx     context.Context
	reader  *bytes.Reader
	release func()
}

func (r *snapshot) Read(p []byte) (int, error) {
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

func (r *snapshot) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.reader != nil {
		r.reader = nil
		if r.release != nil {
			r.release()
			r.release = nil
		}
	}
	return nil
}
